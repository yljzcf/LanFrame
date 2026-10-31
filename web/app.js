'use strict';

const $ = (s) => document.querySelector(s);
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

async function api(path, opts = {}) {
  const r = await fetch(path, opts);
  const text = await r.text();
  if (!r.ok) throw new Error(text.trim() || r.statusText);
  return text ? JSON.parse(text) : null;
}
const post = (path, body) => api(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body ?? {}) });

const video = $('#video');
const overlay = $('#overlay');
const octx = overlay.getContext('2d');

let S = null;           // /api/state 的结果
let cur = null;         // 当前视频
let anns = { items: [], deleted: [] };
let annVersion = null;
let frame = 0;
let focused = null;     // 选中的批注：暂停在它那一帧时显示它的标注
let editor = null;      // 正在写或修改的批注
let videoOffline = false;
let lastListKey = '';
let modalKind = null;

// ---------- 小工具 ----------

const fps = () => (cur && cur.fps > 0 ? cur.fps : 25);
const totalFrames = () => (cur?.frames || Math.round((video.duration || 0) * fps()));
const pad2 = (n) => String(n).padStart(2, '0');
function tc(f) {
  const r = Math.round(fps());
  const s = Math.floor(f / r);
  return [Math.floor(s / 3600), Math.floor(s / 60) % 60, s % 60, f % r].map(pad2).join(':');
}
function fmtSize(n) {
  if (n > 1 << 30) return (n / (1 << 30)).toFixed(1) + ' GB';
  return (n / (1 << 20)).toFixed(1) + ' MB';
}
function fmtTime(ms) {
  const d = new Date(ms);
  return `${d.getMonth() + 1}-${d.getDate()} ${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
}
const round4 = (x) => Math.round(Math.min(1, Math.max(0, x)) * 10000) / 10000;
const nameOf = (id, fallback) => (id === S.me.id ? `${S.me.name}（我）` : (S.peers || []).find((p) => p.id === id)?.name || fallback);
const myAddrs = () => (S.addrs || []).map((a) => `<code>http://${a}:${S.port}</code>`).join(' ') || '（没有检测到局域网地址）';

// ---------- 状态轮询 ----------

async function refresh() {
  let s;
  try { s = await api('/api/state'); } catch { return; }
  S = s;
  if (!s.configured) {
    if (!modalKind) openSetup(true);
    return;
  }
  $('#me').textContent = `${s.me.name} · 设备码 ${s.me.code}`;
  $('#paused-bar').hidden = !s.paused;
  if (renderUpdate()) return;
  renderUpdateSettings();
  $('#share-error').hidden = !s.shareError;
  if ($('#share-error-text').textContent !== (s.shareError || '')) $('#share-error-text').textContent = s.shareError || '';
  renderVideos();
  renderDownloads();
  tryOpenLink();
  if (cur) {
    // 版本信息可能变了（比如刚确认了新版本），同步到当前视频
    const fresh = findVideo(cur.owner, cur.fp).v;
    if (fresh) Object.assign(cur, { name: fresh.name, title: fresh.title, group: fresh.group, version: fresh.version, versions: fresh.versions, missing: fresh.missing, suggest: fresh.suggest, holders: fresh.holders, backup: fresh.backup, local: fresh.local });
    renderTitle();
    renderVersionBar();
    if (videoOffline && sourceOnline(cur.fp)) reloadVideo();
  }
  if (cur && s.annVersion !== annVersion) await loadAnns();
}

// sourceOnline 判断现在能不能播放：自己有、原主人在线，或者有人（包括本机）存着备份在线
function sourceOnline(fp) {
  const ok = (v, online) => v.fp === fp && ((online && !v.missing) || v.holders?.length);
  if ((S.mine || []).some((v) => ok(v, true))) return true;
  return (S.peers || []).some((p) => (p.videos || []).some((v) => ok(v, p.online)));
}

// ---------- 视频列表 ----------

// 按版本组归并，每组最新版本排第一
function groupVersions(list) {
  const groups = new Map();
  for (const v of list) {
    if (!groups.has(v.group)) groups.set(v.group, []);
    groups.get(v.group).push(v);
  }
  const out = [...groups.values()].map((vs) => vs.sort((a, b) => b.version - a.version));
  return out.sort((a, b) => videoName(a[0]).localeCompare(videoName(b[0]), 'zh'));
}

// 文件名去掉扩展名和末尾的版本号，比如 LAX43_EP03_v02.mp4 → LAX43_EP03
const baseName = (name) => name.replace(/\.[^.]+$/, '').replace(/[\s_\-.]*v\d+$/i, '');
// 视频名称：分享者起过名就用名称，否则用文件名去掉扩展名和末尾的版本号
const videoName = (v) => v.title || baseName(v.name) || v.name;
const verLabel = (n) => `v${pad2(n)}`;
// 带版本号的名称，比如 LAX43_EP03_v02，列表和标题都用它。
// 起过名的用"名称_v版本号"；没起名的，文件名里已经带版本号就照用，没带才补上
function versionedName(v) {
  if (v.title) return `${v.title}_${verLabel(v.version)}`;
  const base = v.name.replace(/\.[^.]+$/, '');
  return /[\s_\-.]v\d+$/i.test(base) ? base : `${base}_${verLabel(v.version)}`;
}

// 最新版本是主行，旧版本缩进列在下面。原主人那边已经没有这个文件、显示的是备份时，名字下面标"备份版"
function videoRow(v, ownerId, sub) {
  const n = S.counts?.[v.fp] || 0;
  const active = cur && cur.fp === v.fp && cur.owner === ownerId;
  const path = v.dir && v.dir !== '.' ? `${v.dir}/${v.name}` : v.name;
  const holders = v.holders?.length ? `，备份在${v.holders.join('、')}` : '';
  return `<div class="video-item ${sub ? 'sub' : ''} ${active ? 'active' : ''} ${v.missing ? 'missing' : ''}" data-owner="${esc(ownerId)}" data-fp="${v.fp}">
    <span class="name-col"><span class="name" title="本地文件：${esc(path)}${esc(holders)}">${esc(versionedName(v))}</span>
      ${v.backup ? `<span class="sub-note" title="原主人那边已经没有这个文件，这是同事存着的备份${esc(holders)}">备份版</span>` : ''}</span>
    ${v.missing ? '<span class="ver">文件已不在</span>' : ''}
    ${v.suggest ? '<span class="tag">新版本？</span>' : ''}
    ${n ? `<span class="badge" title="批注数">${n}</span>` : ''}</div>
    ${active ? `<div class="video-tools">${backupButton(v, ownerId)}
      <button data-vtool="share" title="复制一个直接打开这个视频的链接">分享视频</button>
      <button data-vtool="export" title="把这个版本的所有批注导出成一个带截图的网页表格">导出批注</button></div>` : ''}`;
}

// backupButton 是同事的视频下面的"备份视频"：拿一份存到本机共享文件夹的"备份"里，原主人不在时别人也能从你这里看。
// 自己的视频没有这个按钮；已经备份过、正在备份时按钮变灰。
function backupButton(v, ownerId) {
  if (ownerId === S.me.id || v.missing) return '';
  if (v.local) return '<button disabled title="已经备份在你共享文件夹的“备份”里">已备份</button>';
  if ((S.downloads || []).some((d) => d.fp === v.fp && dlActive(d))) return '<button disabled title="左边列表底部能看进度">备份中…</button>';
  return '<button data-vtool="backup" title="拿一份原片存到你共享文件夹的“备份”里，原主人不在时同事也能从你这里看，批注是同一套">备份视频</button>';
}

function renderVideos() {
  const groups = [{ id: S.me.id, name: '我的视频', online: true, videos: S.mine || [], mine: true }];
  for (const p of S.peers || []) groups.push({ id: p.id, name: p.name, online: p.online, videos: p.videos || [], keyMismatch: p.keyMismatch, files: p.files });
  const backing = (S.downloads || []).filter((d) => d.fp && dlActive(d)).map((d) => d.fp); // "备份中…"的按钮跟着变
  const key = JSON.stringify([groups, S.counts, cur?.fp, cur?.owner, backing]);
  if (key === lastListKey) return;
  lastListKey = key;
  let html = groups.map((g) => {
    let items = groupVersions(g.videos).map((vs) => vs.map((v, i) => videoRow(v, g.id, i > 0)).join('')).join('');
    if (!items) {
      const why = g.mine ? '"分享"里还没有视频' : g.keyMismatch ? '团队口令和你的不一样，看不到对方的视频' : '没有分享视频';
      items = `<div class="empty-group">${why}</div>`;
    }
    const files = g.mine ? '<button class="link files-btn" data-open-share title="打开共享文件夹里的“分享”，要给同事看的视频和文件放在这里">打开文件夹</button>'
      : g.online && g.files && !g.keyMismatch
        ? `<button class="link files-btn" data-files="${esc(g.id)}" title="浏览和下载 ${esc(g.name)} 共享文件夹里的文件">文件</button>` : '';
    return `<div class="group-head"><span class="dot ${g.online ? 'on' : ''}"></span>${esc(g.name)}${!g.mine && !g.online ? '（离线）' : ''}${files}</div>${items}`;
  }).join('');
  if (!(S.peers || []).length) {
    html += '<div class="empty-group">还没发现同事的电脑。确认对方已经打开程序；过一会儿还没出现，点右上角"添加用户"输入对方电脑的 IP。</div>';
  }
  $('#video-list').innerHTML = html;
}

$('#video-list').addEventListener('click', (e) => {
  const files = e.target.closest('[data-files]')?.dataset.files;
  if (files) { openFiles(files, ''); return; }
  if (e.target.closest('[data-open-share]')) { post('/api/open-share').catch((err) => alert(err.message)); return; }
  // 当前视频名下面的"备份视频""分享视频""导出批注"
  const tool = e.target.closest('[data-vtool]')?.dataset.vtool;
  if (tool === 'share') { shareVideo(); return; }
  if (tool === 'export') { exportAnnotations(); return; }
  if (tool === 'backup') { backupVideo(); return; }
  const el = e.target.closest('.video-item');
  if (el) selectVideo(el.dataset.owner, el.dataset.fp);
});

function findVideo(ownerId, fp) {
  if (ownerId === S.me.id) return { v: (S.mine || []).find((x) => x.fp === fp), ownerName: S.me.name };
  const p = (S.peers || []).find((x) => x.id === ownerId);
  return { v: p?.videos?.find((x) => x.fp === fp), ownerName: p?.name };
}

function selectVideo(ownerId, fp) {
  if (editor && !confirm('正在写的批注还没发布，确定离开吗？')) return;
  closeEditor();
  const { v, ownerName } = findVideo(ownerId, fp);
  if (!v) return;
  cur = { ...v, owner: ownerId, ownerName };
  focused = null;
  frame = 0;
  annVersion = null;
  anns = { items: [], deleted: [] };
  $('#empty-hint').hidden = true;
  $('#player').hidden = false;
  renderTitle();
  const meta = [];
  if (v.width) meta.push(`${v.width}×${v.height}`);
  if (v.fps) meta.push(`${Math.round(v.fps * 100) / 100} fps`);
  if (v.codec) meta.push(v.codec);
  let warn = '';
  if (!v.fps) warn = '读不到帧率，按 25fps 计算';
  else if (v.vfr) warn = '可变帧率素材，帧号可能不准';
  $('#video-meta').textContent = meta.join(' · ') + (warn ? ` · ${warn}` : '');
  $('#video-meta').classList.toggle('warn', !!warn);
  if (v.missing) {
    videoOffline = false;
    video.removeAttribute('src');
    video.load();
    showMsg(`这个版本的文件已经不在${ownerName}的电脑上了，只能查看批注和截图。`);
  } else {
    reloadVideo();
  }
  renderVersionBar();
  lastListKey = '';
  renderVideos();
  renderComments();
  onFrameChanged();
  loadAnns();
}

function reloadVideo() {
  videoOffline = false;
  showMsg('');
  video.src = `/api/stream/${cur.fp}?owner=${encodeURIComponent(cur.owner)}`;
}

// 标题：先是带版本号的名称，后面灰字是本地文件名
// 控制栏左边：带版本号的名称（自己的视频后面跟着"改名""调整版本"），下面灰字是本地文件名、分享者、大小、版本数
function renderTitle() {
  const mine = cur.owner === S.me.id;
  const title = `<span class="vname" title="${esc(versionedName(cur))}">${esc(versionedName(cur))}</span>`
    + (mine ? '<button class="link" data-ver="rename">改名</button><button class="link" data-ver="adjust">调整版本</button>' : '');
  const sub = [cur.name, `${cur.ownerName} 分享${cur.backup ? '（备份版）' : ''}`, fmtSize(cur.size)];
  if (cur.holders?.length) sub.push(`备份在${cur.holders.join('、')}`);
  if (cur.versions > 1) sub.push(`共 ${cur.versions} 个版本`);
  if ($('#video-title').innerHTML !== title) $('#video-title').innerHTML = title;
  $('#video-sub').textContent = sub.join(' · ');
  $('#video-sub').title = $('#video-sub').textContent;
}

// 视频上方的提示：只在疑似新版本、需要分享者确认时出现
function renderVersionBar() {
  const bar = $('#version-bar');
  let html = '';
  if (cur && cur.owner === S.me.id && cur.suggest) {
    html = `<span>这个文件和「${esc(cur.suggest)}」名字相近，是它的新版本吗？</span>
      <button class="primary" data-ver="yes">是，归为新版本</button><button data-ver="no">不是</button>`;
  }
  if (bar.innerHTML !== html) bar.innerHTML = html;
  bar.hidden = !html;
}

$('#player').addEventListener('click', async (e) => {
  const act = e.target.closest('button')?.dataset.ver;
  if (!act || !cur) return;
  if (act === 'adjust') { openAdjustVersion(); return; }
  if (act === 'rename') { openRename(); return; }
  try {
    await post('/api/versions/answer', { fp: cur.fp, yes: act === 'yes' });
    lastListKey = '';
    await refresh();
  } catch (err) {
    alert(err.message);
  }
});

function openAdjustVersion() {
  const others = groupVersions(S.mine || []).filter((vs) => vs[0].group !== cur.group);
  const options = (cur.versions > 1 ? '<option value="">单独作为一个视频</option>' : '')
    + others.map((vs) => `<option value="${vs[0].group}">${esc(videoName(vs[0]))}（${vs[0].versions} 个版本）</option>`).join('');
  openModal('version', `
    <h2>调整版本</h2>
    <p class="muted">把「${esc(cur.name)}」归到哪个视频下面，作为它的一个版本？版本号按文件修改时间排列，每个版本的批注各自独立。</p>
    <div class="field">${options ? `<select id="vm-group">${options}</select>` : '<p>你没有其他视频可以归入。</p>'}</div>
    <div class="modal-actions"><span class="error" id="vm-err"></span><button id="vm-cancel">取消</button>
      <button class="primary" id="vm-ok" ${options ? '' : 'disabled'}>确定</button></div>`);
  $('#vm-cancel').onclick = closeModal;
  $('#vm-ok').onclick = async () => {
    try {
      await post('/api/versions/move', { fp: cur.fp, group: $('#vm-group').value });
      closeModal();
      lastListKey = '';
      await refresh();
    } catch (e) {
      $('#vm-err').textContent = e.message;
    }
  };
}

const extOf = (name) => (name.match(/\.[^.]+$/) || [''])[0];
const BAD_CHARS = /[\\/:*?"<>|\u0000-\u001f]/;

// 改名：视频名称是整个版本组的名称；可以顺便把本机文件改成"名称_v版本号"
function openRename() {
  const members = (S.mine || []).filter((v) => v.group === cur.group).sort((a, b) => a.version - b.version);
  const latest = members[members.length - 1] || cur;
  // 默认值：已有名称，否则用最新版本的文件名去掉扩展名和末尾的版本号
  const initial = cur.title || baseName(latest.name);
  openModal('rename', `
    <h2>修改视频名称</h2>
    <div class="field"><label>视频名称（同事看到的名字），例如 LAX43_EP03</label>
      <input type="text" id="rn-name" maxlength="100" value="${esc(initial)}"></div>
    <label class="row"><input type="checkbox" id="rn-files" checked> 同时把我电脑上的文件改名为"名称_v版本号"</label>
    <div id="rn-preview" class="muted" style="margin-top:8px"></div>
    <p class="muted">改名不影响批注。如果这个文件被剪辑软件的工程引用，改名后工程里会显示素材离线。</p>
    <div class="modal-actions"><span class="error" id="rn-err"></span><button id="rn-cancel">取消</button><button class="primary" id="rn-ok">保存</button></div>`);
  const check = () => {
    const name = $('#rn-name').value.trim();
    let err = '';
    if (!name) err = '名称不能为空';
    else if (BAD_CHARS.test(name) || name.endsWith('.')) err = '名称不能包含 \\ / : * ? " < > |，也不能以句点结尾';
    $('#rn-err').textContent = err;
    $('#rn-preview').innerHTML = !err && $('#rn-files').checked
      ? members.map((v) => (v.missing
        ? `<div>${verLabel(v.version)}：文件已经不在了，跳过</div>`
        : `<div>${esc(v.name)} → <b>${esc(`${name}_${verLabel(v.version)}${extOf(v.name)}`)}</b></div>`)).join('')
      : '';
    return !err;
  };
  $('#rn-name').addEventListener('input', check);
  $('#rn-files').addEventListener('change', check);
  $('#rn-cancel').onclick = closeModal;
  $('#rn-ok').onclick = async () => {
    if (!check()) return;
    try {
      const r = await post('/api/versions/rename', { group: cur.group, name: $('#rn-name').value.trim(), renameFiles: $('#rn-files').checked });
      closeModal();
      lastListKey = '';
      await refresh();
      if (r.failed?.length) alert(`视频名称已修改，但有文件没能改名：\n${r.failed.join('\n')}`);
    } catch (e) {
      $('#rn-err').textContent = e.message;
    }
  };
  check();
  $('#rn-name').select();
}

function showMsg(t) {
  $('#video-msg').hidden = !t;
  $('#video-msg').textContent = t;
}

video.addEventListener('error', async () => {
  if (!cur || !video.error) return;
  const fp = cur.fp;
  let offline = false;
  try {
    const r = await fetch(video.src, { headers: { Range: 'bytes=0-0' } });
    offline = !r.ok;
  } catch { offline = true; }
  if (!cur || cur.fp !== fp) return;
  videoOffline = offline;
  showMsg(offline
    ? '视频所在的电脑不在线，暂时不能播放。对方上线后会自动恢复，批注和截图仍然可以查看。'
    : `浏览器无法播放这个文件${cur.codec ? `（编码 ${cur.codec}）` : ''}。目前只支持浏览器能直接解码的格式，比如 H.264 编码的 MP4。`);
});

// ---------- 画面尺寸 ----------

function layoutVideo() {
  if (!cur) return;
  const area = $('#video-area');
  const ratio = video.videoWidth && video.videoHeight ? video.videoWidth / video.videoHeight : (cur.width && cur.height ? cur.width / cur.height : 16 / 9);
  let w = area.clientWidth;
  let h = w / ratio;
  if (h > area.clientHeight) {
    h = area.clientHeight;
    w = h * ratio;
  }
  const wrap = $('#video-wrap');
  wrap.style.width = `${w}px`;
  wrap.style.height = `${h}px`;
  const dpr = window.devicePixelRatio || 1;
  overlay.width = Math.round(w * dpr);
  overlay.height = Math.round(h * dpr);
  drawOverlay();
}
new ResizeObserver(layoutVideo).observe($('#video-area'));
video.addEventListener('loadedmetadata', layoutVideo);

// ---------- 帧和播放 ----------

function setFrame(f) {
  if (f === frame) return;
  frame = f;
  onFrameChanged();
}

function onFrameChanged() {
  $('#tc').textContent = tc(frame);
  const total = totalFrames();
  $('#frame-no').textContent = total ? `帧 ${frame} / ${total - 1}` : '';
  $('#tl-progress').style.width = total ? `${Math.min(100, ((frame + 1) / total) * 100)}%` : '0';
  drawOverlay();
}

// requestVideoFrameCallback 能拿到真正显示出来的那一帧的时间
let rvfc = null;
function onVideoFrame(now, meta) {
  setFrame(Math.round(meta.mediaTime * fps()));
  rvfc = video.requestVideoFrameCallback(onVideoFrame);
}
if ('requestVideoFrameCallback' in HTMLVideoElement.prototype) {
  video.addEventListener('loadedmetadata', () => {
    if (rvfc !== null) video.cancelVideoFrameCallback(rvfc);
    rvfc = video.requestVideoFrameCallback(onVideoFrame);
  });
} else {
  const sync = () => setFrame(Math.floor(video.currentTime * fps() + 1e-6));
  video.addEventListener('timeupdate', sync);
  video.addEventListener('seeked', sync);
}

function seekFrame(f) {
  if (!cur) return;
  const total = totalFrames();
  f = Math.max(0, total ? Math.min(f, total - 1) : f);
  video.pause();
  // 跳到这一帧显示区间的中间，避免浮点误差落到前一帧
  video.currentTime = (f + 0.5) / fps();
  setFrame(f);
}

function togglePlay() {
  if (!cur || editor) return;
  if (video.paused) {
    focused = null;
    video.play().catch(() => {});
  } else {
    video.pause();
  }
}

const frameAtTime = () => Math.floor(video.currentTime * fps() + 1e-4);

video.addEventListener('play', () => { $('#btn-play').textContent = '暂停'; drawOverlay(); });
// 播放中的帧回调可能被节流，暂停时以当前时间为准对齐到整帧，保证画面和帧号一致
video.addEventListener('pause', () => {
  $('#btn-play').textContent = '播放';
  if (!video.seeking) seekFrame(frameAtTime());
});
$('#btn-play').onclick = togglePlay;
$('#btn-annotate').onclick = () => openEditor('create');

$('#timeline').addEventListener('click', (e) => {
  const m = e.target.closest('.marker');
  if (m) { focusAnn(m.dataset.id); return; }
  if (editor) return;
  const r = e.currentTarget.getBoundingClientRect();
  seekFrame(Math.floor(((e.clientX - r.left) / r.width) * totalFrames()));
});

document.addEventListener('keydown', (e) => {
  if (e.target.matches('input, textarea') || !cur || modalKind) return;
  if (e.key === ' ') { e.preventDefault(); togglePlay(); }
  else if (e.key === 'ArrowLeft' && !editor) { e.preventDefault(); seekFrame(frame - (e.shiftKey ? 10 : 1)); }
  else if (e.key === 'ArrowRight' && !editor) { e.preventDefault(); seekFrame(frame + (e.shiftKey ? 10 : 1)); }
  else if ((e.key === 'c' || e.key === 'C') && !editor) { e.preventDefault(); openEditor('create'); }
  else if (e.key === 'Escape' && editor) closeEditor();
});

// ---------- 画标注 ----------

function drawStrokes(ctx, strokes, w, h) {
  for (const s of strokes || []) {
    ctx.save();
    ctx.strokeStyle = ctx.fillStyle = s.color || '#ff3b30';
    ctx.lineWidth = Math.max(1.5, ((s.width || 4) * w) / 1000);
    ctx.lineCap = ctx.lineJoin = 'round';
    const P = (s.points || []).map(([x, y]) => [x * w, y * h]);
    if (s.kind === 'pen' && P.length) {
      ctx.beginPath();
      ctx.moveTo(...P[0]);
      for (const p of P.slice(1)) ctx.lineTo(...p);
      if (P.length === 1) ctx.lineTo(P[0][0] + 0.1, P[0][1]);
      ctx.stroke();
    } else if (s.kind === 'rect' && P.length === 2) {
      ctx.strokeRect(P[0][0], P[0][1], P[1][0] - P[0][0], P[1][1] - P[0][1]);
    } else if (s.kind === 'arrow' && P.length === 2) {
      const [[x1, y1], [x2, y2]] = P;
      const ang = Math.atan2(y2 - y1, x2 - x1);
      const head = Math.max(10, ctx.lineWidth * 4);
      ctx.beginPath();
      ctx.moveTo(x1, y1);
      ctx.lineTo(x2, y2);
      ctx.stroke();
      ctx.beginPath();
      ctx.moveTo(x2, y2);
      ctx.lineTo(x2 - head * Math.cos(ang - 0.45), y2 - head * Math.sin(ang - 0.45));
      ctx.lineTo(x2 - head * Math.cos(ang + 0.45), y2 - head * Math.sin(ang + 0.45));
      ctx.closePath();
      ctx.fill();
    } else if (s.kind === 'text' && P.length && s.text) {
      const size = Math.max(10, h * 0.045);
      ctx.font = `600 ${size}px -apple-system, "PingFang SC", "Microsoft YaHei", sans-serif`;
      ctx.textBaseline = 'top';
      const lines = s.text.split('\n');
      const lw = Math.max(...lines.map((l) => ctx.measureText(l).width));
      ctx.fillStyle = 'rgba(0,0,0,.55)';
      ctx.fillRect(P[0][0] - size * 0.2, P[0][1] - size * 0.15, lw + size * 0.4, lines.length * size * 1.25 + size * 0.1);
      ctx.fillStyle = s.color;
      lines.forEach((l, i) => ctx.fillText(l, P[0][0], P[0][1] + i * size * 1.25));
    }
    ctx.restore();
  }
}

function drawOverlay() {
  octx.clearRect(0, 0, overlay.width, overlay.height);
  let strokes = null;
  if (editor) {
    strokes = editor.draft ? editor.strokes.concat([editor.draft]) : editor.strokes;
  } else if (focused && video.paused) {
    const a = anns.items.find((x) => x.id === focused);
    if (a && a.content.frame === frame) strokes = a.content.strokes;
  }
  if (strokes) drawStrokes(octx, strokes, overlay.width, overlay.height);
}

function normPoint(e) {
  const r = overlay.getBoundingClientRect();
  return [round4((e.clientX - r.left) / r.width), round4((e.clientY - r.top) / r.height)];
}

overlay.addEventListener('pointerdown', (e) => {
  if (!editor) return;
  const p = normPoint(e);
  if (editor.tool === 'text') {
    e.preventDefault();
    placeTextInput(e, p);
    return;
  }
  overlay.setPointerCapture(e.pointerId);
  editor.draft = { kind: editor.tool, color: editor.color, width: 4, points: editor.tool === 'pen' ? [p] : [p, p] };
  drawOverlay();
});
overlay.addEventListener('pointermove', (e) => {
  const d = editor?.draft;
  if (!d) return;
  const p = normPoint(e);
  if (d.kind === 'pen') {
    const last = d.points[d.points.length - 1];
    if (Math.hypot(p[0] - last[0], p[1] - last[1]) > 0.002) d.points.push(p);
  } else {
    d.points[1] = p;
  }
  drawOverlay();
});
overlay.addEventListener('pointerup', () => {
  const d = editor?.draft;
  if (!d) return;
  editor.draft = null;
  const a = d.points[0];
  const b = d.points[d.points.length - 1];
  if (d.kind === 'pen' || Math.hypot(a[0] - b[0], a[1] - b[1]) > 0.005) editor.strokes.push(d);
  drawOverlay();
});

let textInput = null;
function placeTextInput(e, p) {
  removeTextInput();
  const wrap = $('#video-wrap');
  const r = wrap.getBoundingClientRect();
  const input = document.createElement('input');
  input.className = 'text-input';
  input.placeholder = '输入文字，回车确定';
  input.style.left = `${Math.min(e.clientX - r.left, r.width - 220)}px`;
  input.style.top = `${e.clientY - r.top}px`;
  wrap.appendChild(input);
  textInput = input;
  input.focus();
  const commit = () => {
    if (textInput !== input) return;
    const t = input.value.trim();
    removeTextInput();
    if (t && editor) {
      editor.strokes.push({ kind: 'text', color: editor.color, width: 4, points: [p], text: t });
      drawOverlay();
    }
  };
  input.addEventListener('keydown', (ev) => {
    ev.stopPropagation();
    if (ev.key === 'Enter') commit();
    if (ev.key === 'Escape') removeTextInput();
  });
  input.addEventListener('blur', commit);
}
function removeTextInput() {
  if (!textInput) return;
  const t = textInput;
  textInput = null;
  t.remove();
}

// ---------- 写批注 ----------

function openEditor(mode, ann) {
  if (!cur || editor) return;
  if (mode === 'create' && !video.videoWidth) {
    alert('视频还没加载出来，暂时不能批注。');
    return;
  }
  if (mode === 'edit') seekFrame(ann.content.frame);
  else if (!video.paused) seekFrame(frameAtTime());
  editor = {
    mode,
    ann: ann?.id,
    frame: mode === 'edit' ? ann.content.frame : frame,
    strokes: structuredClone(ann?.content.strokes || []),
    attachments: [...(ann?.content.attachments || [])],
    tool: 'pen',
    color: '#ff3b30',
    draft: null,
  };
  $('#ed-text').value = ann?.content.text || '';
  $('#ed-frame').textContent = `${mode === 'edit' ? '修改' : '批注'} ${tc(editor.frame)}（帧 ${editor.frame}）`;
  $('#ed-save').textContent = mode === 'edit' ? '保存修改' : '发布批注';
  $('#ed-error').textContent = '';
  $('#editor').hidden = false;
  $('#btn-annotate').disabled = true;
  overlay.classList.add('drawing');
  renderAttachments();
  updateTools();
  drawOverlay();
  $('#ed-text').focus();
}

function closeEditor() {
  editor = null;
  removeTextInput();
  $('#editor').hidden = true;
  $('#btn-annotate').disabled = false;
  overlay.classList.remove('drawing');
  drawOverlay();
}

function updateTools() {
  if (!editor) return;
  document.querySelectorAll('.tool').forEach((b) => b.classList.toggle('active', b.dataset.tool === editor.tool));
  document.querySelectorAll('.color').forEach((b) => b.classList.toggle('active', b.dataset.color === editor.color));
}
document.querySelectorAll('.tool').forEach((b) => { b.onclick = () => { editor.tool = b.dataset.tool; updateTools(); }; });
document.querySelectorAll('.color').forEach((b) => { b.onclick = () => { editor.color = b.dataset.color; updateTools(); }; });
$('#ed-undo').onclick = () => { editor?.strokes.pop(); drawOverlay(); };
$('#ed-cancel').onclick = closeEditor;

async function uploadImage(blob) {
  return (await api('/api/images', { method: 'POST', body: blob })).name;
}

// 逐张上传图片，传好一张交给 add；add 返回 false 表示已经用不上了（比如编辑框关了），后面的不再上传
async function uploadImages(files, add, fail) {
  for (const f of files) {
    if (!f.type.startsWith('image/')) continue;
    try {
      if (add(await uploadImage(f)) === false) return;
    } catch (e) {
      fail(e.message);
    }
  }
}

const addAttachments = (files) => uploadImages(files, (name) => {
  if (!editor) return false;
  editor.attachments.push(name);
  renderAttachments();
}, (msg) => { $('#ed-error').textContent = msg; });
$('#ed-file').onchange = (e) => { addAttachments([...e.target.files]); e.target.value = ''; };
$('#ed-text').addEventListener('paste', (e) => {
  const files = [...(e.clipboardData?.files || [])];
  if (files.length) { e.preventDefault(); addAttachments(files); }
});

function renderAttachments() {
  $('#ed-attachments').innerHTML = editor.attachments
    .map((n, i) => `<div class="att"><img src="/api/images/${n}"><button data-i="${i}" title="移除">×</button></div>`).join('');
}
$('#ed-attachments').addEventListener('click', (e) => {
  const b = e.target.closest('button');
  if (b && editor) { editor.attachments.splice(+b.dataset.i, 1); renderAttachments(); }
});

// 截图取视频原始分辨率的当前帧，存成无损 PNG
async function captureFrame() {
  if (video.seeking) await new Promise((r) => video.addEventListener('seeked', r, { once: true }));
  const c = document.createElement('canvas');
  c.width = video.videoWidth;
  c.height = video.videoHeight;
  c.getContext('2d').drawImage(video, 0, 0);
  const blob = await new Promise((res) => c.toBlob(res, 'image/png'));
  return uploadImage(blob);
}

$('#ed-save').onclick = async () => {
  if (!editor) return;
  const btn = $('#ed-save');
  btn.disabled = true;
  $('#ed-error').textContent = '';
  try {
    const content = { text: $('#ed-text').value.trim(), strokes: editor.strokes, attachments: editor.attachments };
    if (!content.text && !content.strokes.length && !content.attachments.length) {
      throw new Error('批注是空的：写点文字、画一笔或者加一张图');
    }
    if (editor.mode === 'create') {
      const shot = await captureFrame();
      const r = await post('/api/annotations', {
        video: { fp: cur.fp, name: cur.name, owner: cur.owner, ownerName: cur.ownerName, fps: fps() },
        content: { ...content, frame: editor.frame, seconds: editor.frame / fps(), shot },
      });
      focused = r.id;
    } else {
      await post(`/api/annotations/${editor.ann}/edit`, { content });
      focused = editor.ann;
    }
    closeEditor();
    await loadAnns();
  } catch (e) {
    $('#ed-error').textContent = e.message;
  } finally {
    btn.disabled = false;
  }
};

// ---------- 批注列表 ----------

async function loadAnns() {
  if (!cur) return;
  const fp = cur.fp;
  try {
    const r = await api(`/api/annotations?fp=${fp}`);
    if (!cur || cur.fp !== fp) return;
    anns = r;
    annVersion = r.version;
    renderComments();
    drawOverlay();
  } catch { /* 下次轮询再试 */ }
}

function renderComments() {
  const list = $('#comment-list');
  if (!cur) {
    list.innerHTML = '<div class="muted pad">还没有选视频</div>';
    $('#tl-markers').innerHTML = '';
    return;
  }
  const { roots, replies, orphans } = threads();
  const total = totalFrames();
  $('#tl-markers').innerHTML = total
    ? roots.map((a) => `<div class="marker" data-id="${a.id}"
        style="left:${((a.content.frame + 0.5) / total) * 100}%"
        title="${esc(nameOf(a.author, a.authorName))}：${esc(a.content.text.slice(0, 40))}"></div>`).join('')
    : '';
  if (!anns.items.length && !anns.deleted.length) {
    list.innerHTML = '<div class="muted pad">这个视频还没有批注。暂停到想说的那一帧，点"批注这一帧"或按 C。</div>';
    return;
  }
  // 重画前记下回复框的输入位置，重画后还原，同步刷新时不打断正在写的回复
  const box = document.activeElement?.closest?.('.reply-box') ? document.activeElement : null;
  const caret = box ? [box.selectionStart, box.selectionEnd] : null;
  list.innerHTML = sortRoots(roots).map((a) => cardHTML(a, replies.get(a.id) || [])).join('')
    + [...orphans].map(([key, rs]) => orphanHTML(key, rs)).join('')
    + anns.deleted.map((d) => `<div class="deleted-row">${esc(d.authorName)} 删除了一条批注 · ${fmtTime(d.time)}</div>`).join('');
  list.querySelectorAll('canvas[data-id]').forEach(drawThumb);
  if (caret) {
    const ta = list.querySelector('.reply-box textarea');
    ta?.focus();
    ta?.setSelectionRange(...caret);
  }
}

// 批注排序：默认按时间码（程序给的顺序就是），也可以按批注时间。回复始终按时间先后挂在原批注下面
function sortRoots(roots) {
  const mode = $('#sort-anns').value;
  if (mode === 'tc') return roots;
  const dir = mode === 'new' ? -1 : 1;
  return [...roots].sort((x, y) => dir * (x.created - y.created));
}

// 记住这台电脑上选的排序方式（浏览器存不了时就用默认的）
try {
  const saved = localStorage.getItem('annSort');
  if (saved && [...$('#sort-anns').options].some((o) => o.value === saved)) $('#sort-anns').value = saved;
} catch { /* 用默认排序 */ }
$('#sort-anns').addEventListener('change', () => {
  try { localStorage.setItem('annSort', $('#sort-anns').value); } catch { /* 不影响排序 */ }
  renderComments();
});

// 主批注和回复分开：回复挂在原批注下面；原批注被删了，回复归到"原批注已被删除"下面
function threads() {
  const ids = new Set(anns.items.map((a) => a.id));
  const roots = [];
  const replies = new Map();
  const orphans = new Map();
  for (const a of anns.items) {
    if (!a.replyTo) roots.push(a);
    else {
      const m = ids.has(a.replyTo) ? replies : orphans;
      if (!m.has(a.replyTo)) m.set(a.replyTo, []);
      m.get(a.replyTo).push(a);
    }
  }
  for (const m of [replies, orphans]) for (const rs of m.values()) rs.sort((x, y) => x.created - y.created);
  return { roots, replies, orphans };
}

const expanded = new Set(); // 展开了完整内容的卡片
let replyDraft = null;       // 正在写的回复：{ root, edit, text, atts }

// 批注内容：收起时附图只显示成 [图片]；展开时显示全文和附图
function contentHTML(c, open, emptyText) {
  const imgs = c.attachments || [];
  const text = c.text ? esc(c.text) : (imgs.length ? '' : `<span class="muted">${emptyText}</span>`);
  if (!open) return `${imgs.length ? `<span class="img-mark">[图片${imgs.length > 1 ? ` ×${imgs.length}` : ''}]</span> ` : ''}${text}`;
  return `${text}${imgs.length ? `<div class="atts">${imgs.map((n) => `<a href="/api/images/${n}" target="_blank" title="查看原图"><img src="/api/images/${n}"></a>`).join('')}</div>` : ''}`;
}

// 卡片：上面是批注人和时间；下面左边缩略图，右边依次是批注内容、时间码、按钮；回复嵌在卡片下方。
function cardHTML(a, replies) {
  const open = expanded.has(a.id);
  const edited = a.history.length > 1
    ? '<button class="link" data-act="history" title="查看修改历史">已修改</button>' : '';
  return `<div class="card ${open ? 'expanded' : ''} ${focused === a.id ? 'active' : ''}" data-id="${a.id}">
    <div class="card-head">
      <span class="author">${esc(nameOf(a.author, a.authorName))}</span>
      <span class="spacer"></span>${edited}
      <span class="muted">${fmtTime(a.created)}</span>
    </div>
    <div class="card-body">
      <div class="thumb">${a.content.shot ? `<canvas data-id="${a.id}"></canvas>` : '<div class="no-shot">没有截图</div>'}</div>
      <div class="card-main">
        <div class="text" data-act="expand" title="${open ? '点击收起' : '点击展开完整批注'}">${contentHTML(a.content, open, '（只有画面标注）')}${open ? '<div class="fold">收起</div>' : ''}</div>
        <div class="tc">${tc(a.content.frame)}</div>
        <div class="actions">${a.canEdit ? '<button data-act="edit">修改</button><button data-act="delete" class="danger">删除</button>' : ''}<button data-act="reply">回复</button></div>
        <div class="history" hidden></div>
        ${replies.length ? `<div class="replies">${replies.map((r) => replyHTML(r, open)).join('')}</div>` : ''}
        ${replyDraft?.root === a.id ? replyBoxHTML() : ''}
      </div>
    </div>
  </div>`;
}

function replyHTML(r, open) {
  return `<div class="reply" data-id="${r.id}">
    <div class="reply-head"><span class="author">${esc(nameOf(r.author, r.authorName))}</span>
      <span class="muted">${fmtTime(r.created)}${r.history.length > 1 ? ' · 已修改' : ''}</span></div>
    <div class="text" data-act="expand">${contentHTML(r.content, open, '')}</div>
    ${r.canEdit ? '<div class="actions"><button data-act="edit">修改</button><button data-act="delete" class="danger">删除</button></div>' : ''}
  </div>`;
}

// 原批注被删了的回复，单独归成一组
function orphanHTML(key, replies) {
  const open = expanded.has(key);
  return `<div class="card orphan ${open ? 'expanded' : ''}" data-orphan="${key}">
    <div class="orphan-head">原批注已被作者删除</div>
    <div class="replies">${replies.map((r) => replyHTML(r, open)).join('')}</div>
  </div>`;
}

function replyBoxHTML() {
  const d = replyDraft;
  return `<div class="reply-box">
    <textarea placeholder="写回复（可以直接粘贴图片），Ctrl+Enter 发送">${esc(d.text)}</textarea>
    ${d.atts.length ? `<div class="reply-atts">${d.atts.map((n, i) => `<div class="att"><img src="/api/images/${n}"><button data-rb="unatt" data-i="${i}" title="移除">×</button></div>`).join('')}</div>` : ''}
    <div class="row"><span class="error" id="rb-err"></span><span class="spacer"></span>
      <label class="btn">添加图片<input type="file" accept="image/*" multiple hidden data-rb="file"></label>
      <button data-rb="cancel">取消</button><button class="primary" data-rb="send">${d.edit ? '保存' : '发送'}</button></div>
  </div>`;
}

const imgCache = new Map();
function loadImg(name) {
  if (!imgCache.has(name)) {
    imgCache.set(name, new Promise((res, rej) => {
      const i = new Image();
      i.onload = () => res(i);
      i.onerror = () => { imgCache.delete(name); rej(new Error('图片还没同步过来')); };
      i.src = `/api/images/${name}`;
    }));
  }
  return imgCache.get(name);
}

async function drawThumb(cv) {
  const a = anns.items.find((x) => x.id === cv.dataset.id);
  if (!a) return;
  try {
    const img = await loadImg(a.content.shot);
    // 缩略图显示得很小，画 360 宽就够清楚
    const w = 360;
    const h = Math.round((w * img.height) / img.width);
    cv.width = w;
    cv.height = h;
    const ctx = cv.getContext('2d');
    ctx.drawImage(img, 0, 0, w, h);
    drawStrokes(ctx, a.content.strokes, w, h);
  } catch {
    cv.remove();
  }
}

function focusAnn(id) {
  const a = anns.items.find((x) => x.id === id);
  if (!a || editor) return;
  focused = id;
  seekFrame(a.content.frame);
  document.querySelectorAll('.card').forEach((c) => c.classList.toggle('active', c.dataset.id === id));
  drawOverlay();
}

$('#comment-list').addEventListener('click', async (e) => {
  const card = e.target.closest('.card');
  if (!card || e.target.closest('a')) return;
  if (e.target.closest('.reply-box')) { onReplyBoxClick(e); return; }
  const root = anns.items.find((x) => x.id === card.dataset.id); // 原批注被删了的那组回复没有 root
  const a = anns.items.find((x) => x.id === e.target.closest('.reply')?.dataset.id) || root;
  const act = e.target.closest('[data-act]')?.dataset.act;
  if (!act) {
    if (root) focusAnn(root.id);
    return;
  }
  try {
    if (act === 'expand') {
      const key = card.dataset.id || card.dataset.orphan;
      if (expanded.has(key)) expanded.delete(key);
      else expanded.add(key);
      renderComments();
      return;
    }
    if (act === 'history') {
      const box = card.querySelector('.history');
      box.hidden = !box.hidden;
      box.innerHTML = a.history.slice(0, -1).reverse()
        .map((h) => `<div>${fmtTime(h.time)}：${esc(h.content.text || '（没有文字）')}</div>`).join('');
      return;
    }
    if (act === 'reply') {
      if (replyDraft?.root !== root.id) replyDraft = { root: root.id, edit: null, text: '', atts: [] };
      renderComments();
      $(`.card[data-id="${root.id}"] .reply-box textarea`)?.focus();
      return;
    }
    if (act === 'edit') {
      if (a.replyTo) {
        // 回复就在卡片里改，不进画面标注编辑器
        replyDraft = { root: a.replyTo, edit: a.id, text: a.content.text, atts: [...(a.content.attachments || [])] };
        renderComments();
        $(`.card[data-id="${a.replyTo}"] .reply-box textarea`)?.focus();
        return;
      }
      if (editor) return;
      focusAnn(a.id);
      openEditor('edit', a);
      return;
    }
    if (act === 'delete') {
      const what = a.replyTo ? '回复' : '批注';
      const preview = (a.content.text || '（没有文字）').slice(0, 60);
      const ok = await askConfirm(`删除这条${what}？`,
        `「${preview}」\n\n删除后，所有电脑上的这条${what}${a.replyTo ? '' : '和截图'}都会被清除，不能恢复。`, '删除');
      if (!ok) return;
      await post(`/api/annotations/${a.id}/delete`);
      await loadAnns();
    }
  } catch (err) {
    alert(err.message);
  }
});

// ---------- 回复框 ----------

async function onReplyBoxClick(e) {
  const btn = e.target.closest('[data-rb]');
  if (!btn || !replyDraft) return;
  if (btn.dataset.rb === 'cancel') {
    replyDraft = null;
    renderComments();
  } else if (btn.dataset.rb === 'unatt') {
    replyDraft.atts.splice(+btn.dataset.i, 1);
    renderComments();
  } else if (btn.dataset.rb === 'send') {
    sendReply();
  }
}

async function sendReply() {
  const d = replyDraft;
  const text = d.text.trim();
  if (!text && !d.atts.length) {
    $('#rb-err').textContent = '写点文字或者加一张图';
    return;
  }
  try {
    if (d.edit) await post(`/api/annotations/${d.edit}/edit`, { content: { text, attachments: d.atts } });
    else await post('/api/annotations', { replyTo: d.root, content: { text, attachments: d.atts } });
    replyDraft = null;
    await loadAnns();
  } catch (err) {
    $('#rb-err').textContent = err.message;
  }
}

const addReplyImages = (files) => uploadImages(files, (name) => {
  if (!replyDraft) return false;
  replyDraft.atts.push(name);
  renderComments();
}, (msg) => { $('#rb-err').textContent = msg; });

$('#comment-list').addEventListener('input', (e) => {
  if (replyDraft && e.target.matches('.reply-box textarea')) replyDraft.text = e.target.value;
});
$('#comment-list').addEventListener('change', (e) => {
  if (e.target.matches('[data-rb=file]')) {
    addReplyImages([...e.target.files]);
    e.target.value = '';
  }
});
$('#comment-list').addEventListener('paste', (e) => {
  if (!e.target.matches('.reply-box textarea')) return;
  const files = [...(e.clipboardData?.files || [])];
  if (files.length) {
    e.preventDefault();
    addReplyImages(files);
  }
});
$('#comment-list').addEventListener('keydown', (e) => {
  if (e.target.matches('.reply-box textarea') && e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
    e.preventDefault();
    sendReply();
  }
});

// ---------- 分享视频链接 ----------
// 链接指向对方自己电脑上的程序（localhost），打开后直接跳到这个视频的这个版本。

async function shareVideo() {
  if (!cur) return;
  const link = `http://localhost:${S.port}/#${new URLSearchParams({ v: cur.fp, o: cur.owner })}`;
  const what = versionedName(cur);
  const addr = (S.addrs || [])[0];
  const text = `${S.me.name} 分享了视频「${what}」：\n${link}\n`
    + (addr ? `（需要先打开局域网审片程序。还没装的话，用浏览器打开 http://${addr}:${S.port} 下载）` : '（需要先打开局域网审片程序）');
  try {
    await navigator.clipboard.writeText(text);
    toast('链接已复制，可以直接粘贴到飞书、微信');
  } catch {
    openModal('share', `<h2>复制分享链接</h2><textarea class="share-text" readonly>${esc(text)}</textarea>
      <div class="modal-actions"><button class="primary" id="sh-ok">关闭</button></div>`);
    $('.share-text').select();
    $('#sh-ok').onclick = closeModal;
  }
}


// ---------- 导出批注 ----------
// 在网页里把截图和画面标注合成好，生成一个独立的 .html 文件（图片都嵌在里面），浏览器打开就能看，也能打印成 PDF。

// 图片缩到最宽 960 像素、存成 JPEG，导出文件不会太大
async function imageDataURL(name, strokes) {
  const img = await loadImg(name);
  const w = Math.min(960, img.width);
  const h = Math.round((w * img.height) / img.width);
  const c = document.createElement('canvas');
  c.width = w;
  c.height = h;
  const ctx = c.getContext('2d');
  ctx.fillStyle = '#fff';
  ctx.fillRect(0, 0, w, h);
  ctx.drawImage(img, 0, 0, w, h);
  if (strokes) drawStrokes(ctx, strokes, w, h);
  return c.toDataURL('image/jpeg', 0.85);
}

async function exportImgs(names) {
  const out = [];
  for (const n of names || []) {
    try {
      out.push(`<img src="${await imageDataURL(n)}">`);
    } catch {
      out.push('<span class="muted">（图片还没同步过来）</span>');
    }
  }
  return out.length ? `<div class="atts">${out.join('')}</div>` : '';
}

async function exportReplies(rs) {
  const out = [];
  for (const r of rs || []) {
    out.push(`<div class="reply"><b>${esc(nameOf(r.author, r.authorName))}</b> <span class="muted">${fmtTime(r.created)}</span>
      <div class="text">${esc(r.content.text)}</div>${await exportImgs(r.content.attachments)}</div>`);
  }
  return out.join('');
}

let exporting = false;

async function exportAnnotations() {
  if (!cur || exporting) return;
  const { roots, replies, orphans } = threads();
  if (!roots.length && !orphans.size) {
    toast('这个版本还没有批注');
    return;
  }
  exporting = true;
  toast('正在生成导出文件…');
  try {
    const rows = [];
    for (const [i, a] of roots.entries()) {
      let shot = '<span class="muted">没有截图</span>';
      if (a.content.shot) {
        try {
          shot = `<img src="${await imageDataURL(a.content.shot, a.content.strokes)}">`;
        } catch {
          shot = '<span class="muted">（截图还没同步过来）</span>';
        }
      }
      rows.push(`<tr><td>${i + 1}</td>
        <td class="tc">${tc(a.content.frame)}<div class="muted">帧 ${a.content.frame}</div></td>
        <td class="shot">${shot}</td>
        <td><div class="text">${esc(a.content.text)}</div>${await exportImgs(a.content.attachments)}</td>
        <td>${esc(nameOf(a.author, a.authorName))}<div class="muted">${fmtTime(a.created)}${a.history.length > 1 ? ' 已修改' : ''}</div></td>
        <td>${await exportReplies(replies.get(a.id))}</td></tr>`);
    }
    // 原批注被删了的回复：导出时带上回复里存着的原批注内容
    for (const rs of orphans.values()) {
      const q = rs[0].quote;
      rows.push(`<tr><td>—</td><td class="tc">${tc(rs[0].content.frame)}</td><td class="shot"><span class="muted">—</span></td>
        <td><span class="muted">原批注已被删除${q ? `。原文（${esc(q.authorName)}）：` : ''}</span><div class="text">${q ? esc(q.text) : ''}</div></td>
        <td>${q ? esc(q.authorName) : ''}</td><td>${await exportReplies(rs)}</td></tr>`);
    }
    const title = `${versionedName(cur)} 批注`;
    const now = new Date();
    const html = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><title>${esc(title)}</title>
<style>
  body { font: 14px/1.5 -apple-system, "PingFang SC", "Microsoft YaHei", sans-serif; color: #222; margin: 24px; }
  h1 { font-size: 20px; margin: 0 0 4px; }
  .meta, .muted { color: #888; font-size: 12px; }
  .meta { margin-bottom: 16px; }
  table { border-collapse: collapse; width: 100%; }
  th, td { border: 1px solid #d0d0d0; padding: 8px; vertical-align: top; text-align: left; }
  th { background: #f3f4f6; white-space: nowrap; }
  tr { page-break-inside: avoid; }
  .tc { font-family: Menlo, Consolas, monospace; white-space: nowrap; }
  .shot img { max-width: 320px; max-height: 360px; display: block; }
  .text { white-space: pre-wrap; word-break: break-word; }
  .atts img { max-width: 200px; max-height: 200px; margin: 6px 6px 0 0; vertical-align: top; }
  .reply + .reply { margin-top: 8px; padding-top: 8px; border-top: 1px dashed #ddd; }
  @media print { body { margin: 0; } }
</style></head><body>
<h1>${esc(title)}</h1>
<div class="meta">本地文件：${esc(cur.name)} · 分享者：${esc(cur.ownerName)} · 共 ${roots.length} 条批注 · ${esc(S.me.name)} 导出于 ${now.getFullYear()}-${fmtTime(now.getTime())}</div>
<table><thead><tr><th>#</th><th>时间码</th><th>截图</th><th>批注</th><th>批注人</th><th>回复</th></tr></thead>
<tbody>${rows.join('\n')}</tbody></table>
</body></html>`;
    const stamp = `${now.getFullYear()}${pad2(now.getMonth() + 1)}${pad2(now.getDate())}-${pad2(now.getHours())}${pad2(now.getMinutes())}`;
    const filename = `${versionedName(cur)}_批注_${stamp}.html`;
    const url = URL.createObjectURL(new Blob([html], { type: 'text/html' }));
    const link = document.createElement('a');
    link.href = url;
    link.download = filename;
    document.body.appendChild(link);
    link.click();
    link.remove();
    setTimeout(() => URL.revokeObjectURL(url), 10000);
    toast(`已导出：${filename}`);
  } catch (err) {
    alert(`导出失败：${err.message}`);
  } finally {
    exporting = false;
  }
}

let pendingLink = null; // 打开的分享链接要跳到哪个视频

function parseLink() {
  const p = new URLSearchParams(location.hash.slice(1));
  if (!p.get('v')) return;
  pendingLink = { fp: p.get('v'), owner: p.get('o'), since: Date.now() };
  history.replaceState(null, '', location.pathname);
}

// 刚打开程序时同事的电脑可能还没发现，找不到就等几秒再试
function tryOpenLink() {
  const L = pendingLink;
  if (!L || !S?.configured || modalKind) return;
  const owners = [{ id: S.me.id, videos: S.mine || [] }, ...(S.peers || []).map((p) => ({ id: p.id, videos: p.videos || [] }))];
  const hit = owners.find((o) => o.id === L.owner && o.videos.some((v) => v.fp === L.fp))
    || owners.find((o) => o.videos.some((v) => v.fp === L.fp));
  if (!hit) {
    if (Date.now() - L.since > 20000) {
      pendingLink = null;
      alert('找不到分享的视频：分享者可能不在线，或者你们不在同一个局域网、团队口令不一致。');
    } else {
      toast('正在查找分享的视频…');
    }
    return;
  }
  pendingLink = null;
  if (!cur || cur.fp !== L.fp || cur.owner !== hit.id) selectVideo(hit.id, L.fp);
}

window.addEventListener('hashchange', () => { parseLink(); tryOpenLink(); });
parseLink();

let toastTimer = null;
function toast(msg) {
  const t = $('#toast');
  t.textContent = msg;
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.hidden = true; }, 2500);
}

$('#btn-share-retry').onclick = async () => {
  const btn = $('#btn-share-retry');
  btn.disabled = true;
  try {
    await post('/api/rescan');
    toast('共享文件夹已恢复');
  } catch (e) {
    toast('还是打不开，请按上面的说明操作后再试');
  } finally {
    btn.disabled = false;
    lastListKey = '';
    await refresh();
  }
};
$('#btn-share-change').onclick = () => openSetup(false);

$('#btn-resume').onclick = async () => {
  try {
    await post('/api/toggle', { name: 'running', on: true });
    await refresh();
  } catch (e) {
    alert(e.message);
  }
};

// ---------- 自动更新 ----------
// 程序在后台检查、下载新版本，下载好后在顶部提示，用户点"立即更新"才换上（程序会重启几秒钟）。

let seenVersion = null; // 打开网页时程序的版本，变了说明程序已经换成新版本
let updateLater = '';   // 点过"稍后"的版本，这次打开网页期间不再提示
let updatingTo = '';    // 点了"立即更新"、正在等的版本
let updatingSince = 0;
let updateBarKey = '';

function setUpdateBar(text, buttons = '') {
  $('#update-bar').hidden = false;
  if (updateBarKey === text + buttons) return; // 每次轮询都重画的话，按钮会闪
  updateBarKey = text + buttons;
  $('#update-text').textContent = text;
  $('#update-actions').innerHTML = buttons;
}

// renderUpdate 显示顶部的更新提示。程序已经换成新版本时刷新网页，返回 true。
function renderUpdate() {
  if (seenVersion === null) seenVersion = S.version;
  if (S.version !== seenVersion) {
    // 旧网页的脚本可能和新程序对不上，刷新一下。正在写批注或回复时不打断
    const typing = [...document.querySelectorAll('textarea')].some((t) => t.offsetParent && t.value.trim());
    if (!typing) {
      location.reload();
      return true;
    }
    setUpdateBar(`程序已经更新到 ${S.version}。发布完手上的批注后，请刷新页面。`, '<button class="primary" data-act="reload">刷新页面</button>');
    return false;
  }
  const u = S.update || {};
  if (updatingTo) {
    setUpdateBar(Date.now() - updatingSince > 60000
      ? `更新到 ${updatingTo} 好像没有完成。请退出程序后重新打开；还是不行的话，到配置文件夹的 log.txt 里看原因。`
      : `正在更新到 ${updatingTo}，程序会重启几秒钟，请稍候…`);
  } else if (u.ready && u.latest !== updateLater) {
    const later = '<button data-act="later">稍后</button>';
    if (u.blocked) {
      setUpdateBar(`新版本 ${u.latest} 已经下载好，但不能自动换上：${u.blocked}。`,
        (u.download ? `<a class="btn" href="${esc(u.download)}" download>手动下载新版本</a>` : '') + later);
    } else {
      setUpdateBar(`新版本 ${u.latest} 已经下载好。更新时程序会重启几秒钟，正在看你视频的同事会短暂中断。`,
        `<button class="primary" data-act="apply">立即更新</button>${later}`);
    }
  } else {
    $('#update-bar').hidden = true;
  }
  return false;
}

// renderUpdateSettings 更新"软件配置"里程序版本那一行（窗口开着时每次轮询都会刷新）。
function renderUpdateSettings() {
  const box = $('#su-update');
  if (!box) return;
  const u = S.update || {};
  let status;
  let buttons = '';
  if (!u.supported) {
    status = '开发版，不检查更新';
  } else if (updatingTo) {
    status = `正在更新到 ${updatingTo}…`;
  } else if (u.ready) {
    status = `新版本 ${u.latest} 已经下载好${u.blocked ? `，但不能自动换上：${u.blocked}` : ''}`;
    if (!u.blocked) buttons = '<button class="primary" data-act="apply">立即更新</button>';
    else if (u.download) buttons = `<a class="btn" href="${esc(u.download)}" download>手动下载新版本</a>`;
  } else {
    if (u.busy) status = `${u.busy}…`;
    else if (u.error) status = u.error;
    else status = u.checked ? `已是最新（${fmtTime(u.checked)} 检查过）` : '已是最新';
    buttons = `<button data-act="check" ${u.busy ? 'disabled' : ''}>检查更新</button>`;
  }
  const html = `<span>${esc(S.version)}</span><span class="muted">${esc(status)}</span><span class="spacer"></span>${buttons}`;
  if (box.dataset.html !== html) {
    box.innerHTML = html;
    box.dataset.html = html;
  }
}

async function applyUpdate() {
  const to = S.update?.latest;
  try {
    await post('/api/update/apply');
  } catch (e) {
    alert(`更新失败：${e.message}`);
    return;
  }
  updatingTo = to;
  updatingSince = Date.now();
  if (modalKind === 'setup') closeModal();
  renderUpdate();
  // 程序重启期间轮询都会失败，到时间了也要提示"好像没有完成"
  setTimeout(renderUpdate, 61000);
}

async function updateAction(e) {
  const act = e.target.closest('[data-act]')?.dataset.act;
  if (act === 'apply') applyUpdate();
  else if (act === 'reload') location.reload();
  else if (act === 'later') {
    updateLater = S.update?.latest || '';
    $('#update-bar').hidden = true;
  } else if (act === 'check') {
    try {
      await post('/api/update/check');
      await refresh();
    } catch (err) {
      $('#su-err').textContent = err.message;
    }
  }
}
$('#update-actions').onclick = updateAction;

// ---------- 弹窗：软件配置、添加用户、使用说明 ----------

function openModal(kind, html) {
  modalKind = kind;
  $('#modal-box').onclick = null;
  $('#modal-box').classList.toggle('wide', kind === 'files' || kind === 'downloads');
  $('#modal-box').innerHTML = html;
  $('#modal').hidden = false;
}
function closeModal() {
  modalKind = null;
  $('#modal').hidden = true;
  if (confirmResolve) {
    confirmResolve(false);
    confirmResolve = null;
  }
}

// askConfirm 是程序里自己的确认窗口（比浏览器自带的确认框清楚），点确认返回 true，取消或点外面返回 false。
let confirmResolve = null;
function askConfirm(title, body, okText) {
  return new Promise((resolve) => {
    openModal('confirm', `<h2>${esc(title)}</h2><p class="confirm-body">${esc(body)}</p>
      <div class="modal-actions"><button id="cf-no">取消</button><button class="danger-solid" id="cf-yes">${esc(okText)}</button></div>`);
    confirmResolve = resolve;
    $('#cf-no').onclick = closeModal;
    $('#cf-yes').onclick = () => {
      confirmResolve = null;
      closeModal();
      resolve(true);
    };
    $('#cf-no').focus();
  });
}
$('#modal').addEventListener('click', (e) => {
  if (e.target.id === 'modal' && modalKind !== 'setup-first') closeModal();
});

// pickerHTML 和 mountPicker：网页里的文件夹选择器。浏览器拿不到本地路径，由程序列出目录（/api/fs）。
// mountPicker 返回的对象里，path 是当前选中的文件夹，roots 是可选的根目录（个人文件夹、各个磁盘）。
const pickerHTML = (id) => `<div class="picker" id="${id}"><div class="path"></div><div class="roots"></div><div class="dirs"></div></div>`;
function mountPicker(id, start, errEl) {
  const el = $('#' + id);
  const st = { path: start || '', roots: [] };
  async function browse(p, fallback) {
    try {
      const r = await api(`/api/fs?path=${encodeURIComponent(p || '')}`);
      st.path = r.path;
      st.roots = r.roots;
      st.movable = r.movable;
      errEl.textContent = '';
      el.querySelector('.path').textContent = `当前选中：${r.path}`;
      el.querySelector('.roots').innerHTML = r.roots.map((x) => `<button data-p="${esc(x.path)}">${esc(x.label)}</button>`).join('');
      el.querySelector('.dirs').innerHTML = (r.parent ? `<div class="dir" data-p="${esc(r.parent)}">⬆ 上一级</div>` : '')
        + (r.dirs.map((d) => `<div class="dir" data-p="${esc(d.path)}">📁 ${esc(d.name)}</div>`).join('') || '<div class="dir muted">（没有子文件夹）</div>');
    } catch (e) {
      if (fallback) browse('');
      else errEl.textContent = e.message;
    }
  }
  el.onclick = (e) => {
    const t = e.target.closest('[data-p]');
    if (t) browse(t.dataset.p);
  };
  browse(st.path, true); // 起点打不开时（比如还没建的下载文件夹）从个人文件夹开始
  return st;
}

// openSetup 是"软件配置"（第一次打开时是欢迎页）：基本信息、目录位置、开关、本机信息。
// 用户名、团队口令、共享目录点"保存"才生效；开关改了立即生效，第一次设置时不显示。
function openSetup(first) {
  const ips = (S?.addrs || []).map((a) => `<code>${esc(a)}</code>`).join(' ') || '<span class="muted">没有检测到局域网地址</span>';
  openModal(first ? 'setup-first' : 'setup', `
    <h2>${first ? '欢迎使用局域网审片' : '软件配置'}</h2>
    ${first ? '<p class="muted">视频始终留在你自己的电脑上，同事通过局域网直接播放原片、写批注。开始前先填好下面几项：</p>' : ''}
    <div class="section-title">基本信息</div>
    <div class="field"><label>用户名（同事会看到）</label><input type="text" id="su-name" maxlength="20" value="${esc(S?.me?.name || '')}"></div>
    <div class="field"><label>团队口令（可选）：只有填了相同口令的电脑能互相看到视频和批注，团队里每个人都要填一样的；不填就是局域网里谁都能看。</label>
      <input type="text" id="su-key" maxlength="64" placeholder="不填则不设口令" value="${esc(S?.teamKey || '')}"></div>
    <div class="section">
      <div class="section-title">目录位置</div>
      <div class="field"><label>本机共享目录：程序会在里面自动建三个子文件夹——"分享"放要给同事看的视频和文件，"备份"放从同事那里拿来的东西，"批注"放所有批注（电脑之间自动同步，不要手动删改）。目录里原有的东西会挪进"分享"。点文件夹进入，当前所在的就是共享目录。</label>
        ${pickerHTML('su-picker')}
      </div>
    </div>
    ${first ? '' : `<div class="section">
      <div class="section-title">运行选项 <span class="muted">（改了立即生效）</span></div>
      <label class="toggle"><input type="checkbox" data-toggle="autostart" ${S.toggles?.autostart ? 'checked' : ''} ${S.toggles?.autostartSupported ? '' : 'disabled'}>
        <span>开机启动</span></label>
      <label class="toggle"><input type="checkbox" data-toggle="silentStart" ${S.toggles?.silentStart ? 'checked' : ''} ${S.toggles?.autostart ? '' : 'disabled'}>
        <span>静默运行<br><span class="muted">开机启动时不弹出浏览器，只在托盘里待命</span></span></label>
      <label class="toggle"><input type="checkbox" data-toggle="running" ${S.paused ? '' : 'checked'}>
        <span>启动服务<br><span class="muted">关掉后程序留在后台，但不分享视频、不同步批注，同事那边显示你离线</span></span></label>
      <label class="toggle"><input type="checkbox" data-toggle="fileShare" ${S.fileShare ? 'checked' : ''}>
        <span>允许同事下载我共享文件夹里的所有文件<br><span class="muted">同事能浏览和下载你的"分享"和"备份"、备份你的视频，只能拿，不能改、不能删；"批注"和隐藏文件除外。关掉后同事仍然能看你的视频</span></span></label>
    </div>`}
    <div class="section">
      <div class="section-title">本机信息</div>
      <div class="info-row"><span class="muted">设备代码</span><b>${esc(S?.me?.code || '')}</b></div>
      <div class="info-row"><span class="muted">本机 IP</span><span>${ips}</span></div>
      <div class="info-row"><span class="muted">程序版本</span>${first ? `<span>${esc(S?.version || '')}</span>` : '<div class="row" id="su-update"></div>'}</div>
    </div>
    <div class="modal-actions"><span class="error" id="su-err"></span>
      ${first ? '' : '<button id="su-cancel">取消</button>'}<button class="primary" id="su-ok">${first ? '开始使用' : '保存'}</button></div>`);
  const share = mountPicker('su-picker', S?.shareDir || '', $('#su-err'));
  $('#su-cancel')?.addEventListener('click', closeModal);
  $('#su-update')?.addEventListener('click', updateAction);
  renderUpdateSettings();
  document.querySelectorAll('[data-toggle]').forEach((box) => box.addEventListener('change', async () => {
    try {
      await post('/api/toggle', { name: box.dataset.toggle, on: box.checked });
      await refresh();
      const silent = document.querySelector('[data-toggle=silentStart]');
      if (silent) silent.disabled = !S.toggles?.autostart;
      $('#su-err').textContent = '';
    } catch (e) {
      box.checked = !box.checked;
      $('#su-err').textContent = e.message;
    }
  }));
  $('#su-ok').addEventListener('click', async () => {
    const nickname = $('#su-name').value.trim();
    if (!nickname) { $('#su-err').textContent = '先填一个用户名'; return; }
    if (share.roots.some((x) => x.path === share.path) && !confirm('共享整个个人文件夹或整个磁盘，会扫描里面所有视频，里面的所有文件同事也都能下载。建议新建一个专门的文件夹。仍然继续吗？')) return;
    if (share.path !== S?.shareDir && share.movable > 0
      && !confirm(`这个文件夹里原有的 ${share.movable} 项会挪进新建的"分享"子文件夹。剪辑软件工程里引用了这些文件的，会显示素材离线，需要重新链接。继续吗？`)) return;
    try {
      await post('/api/setup', { nickname, shareDir: share.path, teamKey: $('#su-key').value.trim() });
      closeModal();
      lastListKey = '';
      await refresh();
      if (first) openHelp();
    } catch (e) {
      $('#su-err').textContent = e.message;
    }
  });
}

function openAddPeer() {
  openModal('peer', `
    <h2>添加用户</h2>
    <p class="muted">同一个局域网里打开了程序的同事一般会自动出现。如果过了一会儿还没出现（比如不在同一个网段，或者公司网络屏蔽了自动发现），在这里输入对方电脑的 IP 地址。</p>
    <div class="field"><label>对方的 IP 地址</label><input type="text" id="ap-addr" placeholder="例如 192.168.1.20"></div>
    <p class="muted">你的地址：${myAddrs()}</p>
    <div class="modal-actions"><span class="error" id="ap-err"></span><button id="ap-cancel">取消</button><button class="primary" id="ap-ok">添加</button></div>`);
  $('#ap-cancel').onclick = closeModal;
  const ok = async () => {
    try {
      const r = await post('/api/peers', { addr: $('#ap-addr').value });
      closeModal();
      lastListKey = '';
      await refresh();
      alert(`已添加：${r.name}`);
    } catch (e) {
      $('#ap-err').textContent = e.message;
    }
  };
  $('#ap-ok').onclick = ok;
  $('#ap-addr').addEventListener('keydown', (e) => { if (e.key === 'Enter') ok(); });
  $('#ap-addr').focus();
}

function openHelp() {
  openModal('help', `
    <h2>使用说明</h2>
    <h3>看片</h3>
    <ol>
      <li>左边列出你和同事分享的视频，绿点表示对方在线。点一下就能播放，播放的是对方电脑上的原片，不压缩。</li>
      <li>空格播放/暂停，← → 逐帧移动，按住 Shift 一次移动 10 帧。</li>
    </ol>
    <h3>写批注</h3>
    <ol>
      <li>暂停到想说的那一帧，点"批注这一帧"或按 C。</li>
      <li>在画面上用自由画、箭头、方框、文字标出位置，下面写文字；可以添加图片，也可以直接粘贴截图。</li>
      <li>点"发布批注"。所有人都会看到。右边点一条批注的缩略图或时间码会跳到那一帧并显示标注；批注太长时点文字可以展开全文和附图。</li>
    </ol>
    <h3>批注的规则</h3>
    <ul>
      <li>自己写的批注可以修改（保留历史）和删除，删除会同步清掉所有电脑上的这条批注。</li>
      <li>共享文件夹里有三个子文件夹：要给同事看的视频和文件放进"分享"；"备份"放从同事那里拿的东西；"批注"放所有批注，电脑之间自动同步，请不要手动删改。</li>
    </ul>
    <h3>让同事加入</h3>
    <ol>
      <li>让同事在浏览器里打开 ${myAddrs()}，下载程序。</li>
      <li>双击运行，按提示允许防火墙和本地网络访问。</li>
    </ol>
    <h3>版本</h3>
    <ul>
      <li>重新导出的文件如果和之前的视频名字相近（比如同名覆盖、_v2、(1)），会问你是不是新版本。确认后列在同一个视频下面，旧版本的批注保留。</li>
      <li>自己的视频可以点"调整版本"，手动归到别的视频下面或单独拆出来。</li>
      <li>自己的视频可以点"改名"起一个视频名称（比如 LAX43_EP03），还可以顺便把电脑上的文件改成"名称_v版本号"（比如 LAX43_EP03_v02.mp4）。改名不影响批注。</li>
    </ul>
    <h3>从同事电脑拿文件</h3>
    <ul>
      <li>左边同事名字旁点"文件"，能浏览他共享文件夹里的所有文件，勾选后点"下载"，整个文件夹也能一次拿走。对方不用在电脑前操作。</li>
      <li>下载在后台进行，关掉这个网页也会继续；断网、对方离线或者程序重启后，从断点接着下。左边列表底部的"下载中"点开能看进度；不要了的点"删除"，会移到废纸篓（Windows 是回收站）。</li>
      <li>拿来的东西存在你共享文件夹的"备份"里，按来源分文件夹。拿来的视频成为原主人视频的备份：列表里仍然在原主人名下，不算你的视频；原主人不在时，同事能从你这里播放原片，批注也是同一套。原主人已经删了的，列表里显示备份版，文件名下面标"备份版"。</li>
      <li>只想备份某个视频：选中同事的视频，名字下面点"备份视频"。</li>
      <li>同事也能这样拿你共享文件夹里的文件（只能拿，不能改、不能删）；不想开放的话在"软件配置"里关掉。</li>
    </ul>
    <h3>注意</h3>
    <ul>
      <li>程序没有窗口，界面就是这个网页。托盘里有它的图标（Mac 在屏幕顶部菜单栏，Windows 在右下角），可以打开界面、暂停服务、设置开机启动、退出；再次双击程序也会重新打开这个页面。</li>
      <li>左侧当前视频名下面的"分享视频"会复制一段文字，同事点里面的链接会直接打开这个视频。对方要先打开审片程序。</li>
      <li>每条批注都可以"回复"，回复显示在原批注下面，时间码和原批注一样。</li>
      <li>"导出批注"会把当前版本的所有批注（带截图和回复）导出成一个网页文件，浏览器打开就能看，也能打印成 PDF。</li>
      <li>分享视频期间不要退出程序，也不要合上笔记本让电脑睡眠，否则同事播不了你的视频。</li>
      <li>浏览器只能播放 H.264 等常见编码，ProRes 等格式暂不支持。</li>
      <li>程序会在后台下载新版本，下载好后顶部会提示，点"立即更新"程序重启几秒钟就换好了。</li>
    </ul>
    <div class="modal-actions"><button class="primary" id="help-ok">知道了</button></div>`);
  $('#help-ok').onclick = closeModal;
}

// ---------- 从同事电脑拿文件 ----------
// 同事共享文件夹里的文件都能浏览和下载（只读）。下载由程序在后台做：网页关了也继续，断了从断点接着下。

let fileSel = null; // 文件窗口：{ peer, path, checked: 路径 -> 条目 }，在不同文件夹里勾的都算

const fmtBytes = (n) => (n >= 1 << 30 ? `${(n / (1 << 30)).toFixed(1)} GB` : n >= 1 << 20 ? `${(n / (1 << 20)).toFixed(1)} MB` : n >= 1024 ? `${Math.round(n / 1024)} KB` : `${n} B`);

async function openFiles(peerId, dir) {
  const peer = (S.peers || []).find((p) => p.id === peerId);
  if (!peer) return;
  if (!fileSel || fileSel.peer !== peerId || modalKind !== 'files') fileSel = { peer: peerId, checked: new Map() };
  fileSel.path = dir;
  if (modalKind !== 'files') {
    openModal('files', `<h2>${esc(peer.name)} 的文件</h2>
      <div class="crumbs" id="fl-crumbs"></div>
      <div class="file-list" id="fl-list"></div>
      <p class="muted">存到你共享文件夹的"备份/${esc(peer.name)}"里。视频会成为备份，${esc(peer.name)}不在时同事也能从你这里看。</p>
      <div class="modal-actions"><span class="error" id="fl-err"></span><button id="fl-close">关闭</button><button class="primary" id="fl-ok" disabled>下载</button></div>`);
    $('#fl-close').onclick = closeModal;
    $('#fl-ok').onclick = startDownload;
    $('#fl-crumbs').onclick = (e) => {
      const c = e.target.closest('[data-crumb]');
      if (c) openFiles(fileSel.peer, c.dataset.crumb);
    };
    $('#fl-list').onclick = (e) => {
      const row = e.target.closest('[data-path]');
      if (!row) return;
      const entry = fileSel.entries.find((x) => x.path === row.dataset.path);
      // 点文件夹的名字进入；点勾选框或者文件的名字是勾选
      if (entry.dir && !e.target.closest('input')) { openFiles(fileSel.peer, entry.path); return; }
      if (!e.target.closest('input')) row.querySelector('input').checked = !row.querySelector('input').checked;
      if (row.querySelector('input').checked) fileSel.checked.set(entry.path, entry);
      else fileSel.checked.delete(entry.path);
      renderFileSel();
    };
  }
  const parts = dir ? dir.split('/') : [];
  $('#fl-crumbs').innerHTML = [`<span class="crumb" data-crumb="">共享文件夹</span>`,
    ...parts.map((name, i) => `<span class="crumb" data-crumb="${esc(parts.slice(0, i + 1).join('/'))}">${esc(name)}</span>`)].join('<span class="muted"> / </span>');
  $('#fl-list').innerHTML = '<div class="muted pad">正在读取…</div>';
  $('#fl-err').textContent = '';
  try {
    fileSel.entries = await api(`/api/peer-files?peer=${encodeURIComponent(peerId)}&path=${encodeURIComponent(dir)}`);
  } catch (e) {
    fileSel.entries = [];
    $('#fl-list').innerHTML = '';
    $('#fl-err').textContent = e.message;
    return;
  }
  if (modalKind !== 'files' || fileSel.path !== dir) return; // 读的时候已经关了窗口或者换了文件夹
  $('#fl-list').innerHTML = fileSel.entries.map((f) => `<div class="file-row ${f.dir ? 'is-dir' : ''}" data-path="${esc(f.path)}">
      <input type="checkbox" ${fileSel.checked.has(f.path) ? 'checked' : ''}>
      <span class="fname" title="${f.dir ? '点名字打开这个文件夹' : esc(f.name)}">${f.dir ? '📁' : '📄'} ${esc(f.name)}</span>
      <span class="muted fsize">${f.dir ? '' : fmtBytes(f.size)}</span>
      <span class="muted ftime">${fmtTime(f.mtime)}</span></div>`).join('')
    || '<div class="muted pad">这个文件夹是空的</div>';
  renderFileSel();
}

function renderFileSel() {
  const items = [...fileSel.checked.values()];
  const dirs = items.filter((f) => f.dir).length;
  const bytes = items.reduce((n, f) => n + (f.dir ? 0 : f.size), 0);
  $('#fl-ok').disabled = !items.length;
  $('#fl-ok').textContent = !items.length ? '下载'
    : `下载 ${items.length} 项${dirs ? `（含 ${dirs} 个文件夹）` : `（${fmtBytes(bytes)}）`}`;
}

async function startDownload() {
  const paths = [...fileSel.checked.keys()];
  $('#fl-ok').disabled = true;
  $('#fl-err').textContent = '正在准备…';
  try {
    await post('/api/downloads', { peer: fileSel.peer, paths });
  } catch (e) {
    $('#fl-err').textContent = e.message;
    renderFileSel();
    return;
  }
  closeModal();
  await refresh();
  toast(`开始下载 ${paths.length} 项，左边列表底部能看进度`);
}

const dlStates = {
  queued: '排队中', running: '下载中', waiting: '等对方上线，上线后自动接着下', paused: '服务已暂停，恢复后接着下',
  retrying: '网络不稳，正在重试', done: '已下载', partial: '部分文件没下载成功', failed: '下载失败', canceled: '已取消',
};
const dlActive = (d) => ['queued', 'running', 'waiting', 'paused', 'retrying'].includes(d.state);

function fmtLeft(sec) {
  if (sec < 60) return `${Math.ceil(sec)} 秒`;
  if (sec < 3600) return `${Math.ceil(sec / 60)} 分钟`;
  return `${(sec / 3600).toFixed(1)} 小时`;
}

let dlSeen = null; // 已经提示过"下载完了"的下载

// renderDownloads 在每次轮询时更新：顶栏"下载"上的数字、下载完的提示、开着的下载列表、第一次提示开放文件。
function renderDownloads() {
  const list = S.downloads || [];
  const active = list.filter(dlActive).length;
  // 侧栏底部的下载条：有下载记录就显示；全部结束、清除以后就没了
  const failed = list.filter((d) => d.state === 'failed' || d.state === 'partial').length;
  const strip = $('#dl-strip');
  strip.hidden = !list.length;
  strip.className = active ? '' : failed ? 'failed' : 'idle';
  const html = active ? `下载中 <span class="badge">${active}</span> 项` : failed ? `${failed} 项没下载成功` : '下载记录';
  if (strip.dataset.html !== html) {
    strip.innerHTML = `${html}<span class="spacer"></span>查看`;
    strip.dataset.html = html;
  }
  // 刚打开网页时已经下完的不再提示
  if (dlSeen === null) dlSeen = new Set(list.filter((d) => d.doneAt).map((d) => d.id));
  const done = list.filter((d) => d.doneAt && !dlSeen.has(d.id));
  if (done.length) {
    done.forEach((d) => dlSeen.add(d.id));
    const fresh = done.find((d) => d.state !== 'done') || done[0]; // 有没下载成功的先说它
    const ok = fresh.state === 'done';
    const more = done.length > 1 ? `等 ${done.length} 项` : '';
    $('#dl-done-text').textContent = ok ? `已从 ${fresh.peerName} 拿到「${fresh.name}」${more}` : `从 ${fresh.peerName} 拿「${fresh.name}」${dlStates[fresh.state]}`;
    $('#dl-done-actions').innerHTML = `<button class="primary" data-dl-act="reveal" data-id="${esc(fresh.id)}">${ok ? '打开所在文件夹' : '看看'}</button><button data-dl-act="hide">关闭</button>`;
    $('#dl-done-bar').hidden = false;
  }
  $('#fileshare-bar').hidden = !S.fileShareNotice;
  if (S.fileShareNotice) {
    const text = '共享文件夹分成了三个子文件夹："分享"放给同事看的视频和文件，"备份"放从同事那里拿的东西，"批注"放批注（不要手动删改）。'
      + (S.layoutNote || '') + (S.fileShare ? '共享文件夹里的文件同事都能浏览和下载，只能拿，不能改、不能删；不想开放可以在"软件配置"里关掉。' : '');
    if ($('#fileshare-text').textContent !== text) $('#fileshare-text').textContent = text;
  }
  if (modalKind === 'downloads') renderDownloadList();
}

function openDownloads() {
  openModal('downloads', `<h2>下载</h2>
    <div class="row"><span class="muted">存在共享文件夹的"备份"里，按来源分文件夹</span><span class="spacer"></span><button data-dl-act="open-dir">打开文件夹</button></div>
    <div id="dl-list" class="dl-list"></div>
    <div class="modal-actions"><span class="error" id="dl-err"></span><button data-dl-act="clear">清除已结束的</button><button class="primary" id="dl-close">关闭</button></div>`);
  $('#dl-close').onclick = closeModal;
  $('#modal-box').onclick = dlAction;
  renderDownloadList();
}

function renderDownloadList() {
  const list = S.downloads || [];
  const html = list.map((d) => {
    const pct = d.size ? Math.min(100, (d.got / d.size) * 100) : (d.state === 'done' ? 100 : 0);
    const info = [`${fmtBytes(d.got)} / ${fmtBytes(d.size)}`];
    if (d.dir) info.push(`${d.done} / ${d.files} 个文件`);
    if (d.state === 'running' && d.speed > 0) info.push(`${fmtBytes(Math.round(d.speed))}/秒`, `还要 ${fmtLeft((d.size - d.got) / d.speed)}`);
    const btns = [];
    if (dlActive(d)) btns.push(`<button data-dl-act="cancel" data-id="${esc(d.id)}">取消</button>`);
    if (['failed', 'partial', 'canceled'].includes(d.state)) btns.push(`<button data-dl-act="retry" data-id="${esc(d.id)}">重试</button>`);
    if (d.state !== 'canceled') btns.push(`<button data-dl-act="reveal" data-id="${esc(d.id)}">打开所在文件夹</button>`);
    if (!dlActive(d)) btns.push(`<button class="danger" data-dl-act="delete" data-id="${esc(d.id)}">删除</button>`);
    const errs = [d.err, ...(d.failed || [])].filter(Boolean);
    return `<div class="dl-item ${d.state}">
      <div class="row"><b class="dl-name">${d.dir ? '📁' : '📄'} ${esc(d.name)}</b><span class="muted">来自 ${esc(d.peerName)}</span><span class="spacer"></span><span class="dl-state">${dlStates[d.state] || d.state}</span></div>
      <div class="progress"><div style="width:${pct.toFixed(1)}%"></div></div>
      <div class="row"><span class="muted">${info.join(' · ')}</span><span class="spacer"></span>${btns.join('')}</div>
      ${errs.map((e) => `<div class="error dl-err">${esc(e)}</div>`).join('')}
    </div>`;
  }).join('') || '<div class="muted pad">还没有下载过文件。在左边同事名字旁点"文件"，就能浏览和下载他共享文件夹里的文件。</div>';
  const box = $('#dl-list');
  if (box && box.dataset.html !== html) {
    box.innerHTML = html;
    box.dataset.html = html;
  }
}

async function dlAction(e) {
  const btn = e.target.closest('[data-dl-act]');
  if (!btn) return;
  const act = btn.dataset.dlAct;
  if (act === 'hide') { $('#dl-done-bar').hidden = true; return; }
  if (act === 'delete') { deleteDownload(btn.dataset.id); return; }
  try {
    if (act === 'open-dir') await post('/api/downloads/open');
    else if (act === 'clear') await post('/api/downloads/clear');
    else await post(`/api/downloads/${encodeURIComponent(btn.dataset.id)}/${act}`);
    if (act === 'reveal' && btn.closest('#dl-done-bar')) $('#dl-done-bar').hidden = true;
    await refresh();
  } catch (err) {
    if ($('#dl-err')) $('#dl-err').textContent = err.message;
    else alert(err.message);
  }
}
$('#dl-done-actions').onclick = dlAction;

// deleteDownload 把一项下载拿到的东西移到废纸篓（Windows 是回收站），这条下载记录也拿掉。
// 确认窗口会盖住下载列表，确认或取消后再把下载列表打开。
async function deleteDownload(id) {
  const d = (S.downloads || []).find((x) => x.id === id);
  if (!d) return;
  const trash = S.os === 'windows' ? '回收站' : '废纸篓';
  let body = !d.done ? '还没有下载到文件，只删掉这条下载记录。'
    : `${d.dir ? `文件夹「${d.name}」里已经下好的 ${d.done} 个文件` : `「${d.name}」`}会移到${trash}，需要时可以从${trash}里找回来。这条下载记录也一起删掉。`;
  if (d.fp && d.done) body += `\n\n这是 ${d.peerName} 的视频的备份，删除后，${d.peerName} 不在时同事就不能从你这里看这个视频了。`;
  const ok = await askConfirm('删除下载的文件', body, d.done ? `移到${trash}` : '删除记录');
  let err = '';
  if (ok) {
    try {
      await post(`/api/downloads/${encodeURIComponent(id)}/delete`);
      lastListKey = '';
      await refresh();
    } catch (e) {
      err = e.message;
    }
  }
  openDownloads();
  if (err) $('#dl-err').textContent = err;
  else if (ok && d.done) toast(`已移到${trash}`);
}

async function backupVideo() {
  if (!cur) return;
  try {
    await post('/api/backup', { owner: cur.owner, fp: cur.fp });
  } catch (e) {
    alert(e.message);
    return;
  }
  lastListKey = '';
  await refresh();
  toast(`开始备份，存到你共享文件夹的"备份/${cur.ownerName}"里，左边列表底部能看进度`);
}
$('#dl-strip').onclick = () => S?.configured && openDownloads();

// 从 0.3.x 更新上来的用户，提示一次共享文件夹里的文件同事都能下载了
$('#fileshare-actions').onclick = async (e) => {
  const act = e.target.closest('[data-act]')?.dataset.act;
  if (!act) return;
  try {
    await post('/api/toggle', { name: 'fileShareNotified', on: true });
    await refresh();
  } catch (err) {
    alert(err.message);
    return;
  }
  if (act === 'settings') openSetup(false);
};

$('#btn-add-peer').onclick = () => S?.configured && openAddPeer();
$('#btn-help').onclick = () => S?.configured && openHelp();
$('#btn-settings').onclick = () => S?.configured && openSetup(false);

refresh();
setInterval(refresh, 2000);
