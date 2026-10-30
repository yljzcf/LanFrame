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
  tryOpenLink();
  if (cur) {
    // 版本信息可能变了（比如刚确认了新版本），同步到当前视频
    const fresh = findVideo(cur.owner, cur.fp).v;
    if (fresh) Object.assign(cur, { name: fresh.name, title: fresh.title, group: fresh.group, version: fresh.version, versions: fresh.versions, missing: fresh.missing, suggest: fresh.suggest });
    renderTitle();
    renderVersionBar();
    if (videoOffline && sourceOnline(cur.fp)) reloadVideo();
  }
  if (cur && s.annVersion !== annVersion) await loadAnns();
}

function sourceOnline(fp) {
  if ((S.mine || []).some((v) => v.fp === fp && !v.missing)) return true;
  return (S.peers || []).some((p) => p.online && (p.videos || []).some((v) => v.fp === fp && !v.missing));
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

// 最新版本是主行，旧版本缩进列在下面
function videoRow(v, ownerId, sub) {
  const n = S.counts?.[v.fp] || 0;
  const active = cur && cur.fp === v.fp && cur.owner === ownerId;
  const path = v.dir && v.dir !== '.' ? `${v.dir}/${v.name}` : v.name;
  return `<div class="video-item ${sub ? 'sub' : ''} ${active ? 'active' : ''} ${v.missing ? 'missing' : ''}" data-owner="${esc(ownerId)}" data-fp="${v.fp}">
    <span class="name" title="本地文件：${esc(path)}">${esc(versionedName(v))}</span>
    ${v.missing ? '<span class="ver">文件已不在</span>' : ''}
    ${v.suggest ? '<span class="tag">新版本？</span>' : ''}
    ${n ? `<span class="badge" title="批注数">${n}</span>` : ''}</div>
    ${active ? `<div class="video-tools">
      <button data-vtool="share" title="复制一个直接打开这个视频的链接">分享视频</button>
      <button data-vtool="export" title="把这个版本的所有批注导出成一个带截图的网页表格">导出批注</button></div>` : ''}`;
}

function renderVideos() {
  const groups = [{ id: S.me.id, name: '我的视频', online: true, videos: S.mine || [], mine: true }];
  for (const p of S.peers || []) groups.push({ id: p.id, name: p.name, online: p.online, videos: p.videos || [], keyMismatch: p.keyMismatch });
  const key = JSON.stringify([groups, S.counts, cur?.fp, cur?.owner]);
  if (key === lastListKey) return;
  lastListKey = key;
  let html = groups.map((g) => {
    let items = groupVersions(g.videos).map((vs) => vs.map((v, i) => videoRow(v, g.id, i > 0)).join('')).join('');
    if (!items) {
      const why = g.mine ? '共享文件夹里还没有视频' : g.keyMismatch ? '团队口令和你的不一样，看不到对方的视频' : '没有分享视频';
      items = `<div class="empty-group">${why}</div>`;
    }
    return `<div class="group-head"><span class="dot ${g.online ? 'on' : ''}"></span>${esc(g.name)}${!g.mine && !g.online ? '（离线）' : ''}</div>${items}`;
  }).join('');
  if (!(S.peers || []).length) {
    html += '<div class="empty-group">还没发现同事的电脑。确认对方已经打开程序；过一会儿还没出现，点右上角"添加电脑"输入对方 IP。</div>';
  }
  $('#video-list').innerHTML = html;
}

$('#video-list').addEventListener('click', (e) => {
  // 当前视频名下面的"分享视频""导出批注"
  const tool = e.target.closest('[data-vtool]')?.dataset.vtool;
  if (tool === 'share') { shareVideo(); return; }
  if (tool === 'export') { exportAnnotations(); return; }
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
  const sub = [cur.name, `${cur.ownerName} 分享`, fmtSize(cur.size)];
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

// renderUpdateSettings 更新"设置"里程序版本那一行（设置窗口开着时每次轮询都会刷新）。
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
  const html = `<span class="muted">程序版本 ${esc(S.version)} · ${esc(status)}</span><span class="spacer"></span>${buttons}`;
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

// ---------- 弹窗：设置、添加电脑、使用说明 ----------

function openModal(kind, html) {
  modalKind = kind;
  $('#modal-box').onclick = null;
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

function openSetup(first) {
  let path = S?.shareDir || '';
  let roots = [];
  openModal(first ? 'setup-first' : 'setup', `
    <h2>${first ? '欢迎使用局域网审片' : '设置'}</h2>
    ${first ? '<p class="muted">视频始终留在你自己的电脑上，同事通过局域网直接播放原片、写批注。开始前先做两件事：</p>' : ''}
    <div class="field"><label>1. 你的昵称（同事会看到）</label><input type="text" id="su-name" maxlength="20" value="${esc(S?.me?.name || '')}"></div>
    <div class="field"><label>2. 共享文件夹：放要给同事看的视频，批注也会存在它里面的"批注同步(别删)"文件夹。点文件夹进入，当前所在的就是要共享的文件夹。</label>
      <div class="picker"><div class="path" id="su-path"></div><div class="roots" id="su-roots"></div><div class="dirs" id="su-dirs"></div></div>
    </div>
    <div class="field"><label>3. 团队口令（可选）：设置后，只有填了相同口令的电脑能看到你的视频和批注。团队里每个人都要填一样的；不填就是局域网里谁都能看。</label>
      <input type="text" id="su-key" maxlength="64" placeholder="不填则不设口令" value="${esc(S?.teamKey || '')}"></div>
    <div class="section">
      <div class="row"><span class="muted">你的设备码：<b id="su-code">${esc(S?.me?.code || '')}</b>${first && S?.version ? ` · 程序版本 ${esc(S.version)}` : ''}</span>
        <span class="spacer"></span>
        ${first ? '' : '<a class="btn" href="/api/identity" download>导出身份文件</a>'}
        <label class="btn">${first ? '在别的电脑用过？导入身份文件' : '导入身份文件'}<input type="file" id="su-import" accept=".json,application/json" hidden></label></div>
      <p class="muted">换电脑时，在旧电脑导出身份文件、在新电脑导入，你写过的批注仍然可以修改和删除。身份文件相当于你的签名，不要发给别人。</p>
    </div>
    ${first ? '' : `<div class="section row" id="su-update"></div>
    <div class="section">
      <div class="muted" style="margin-bottom:8px">后台运行（托盘图标的菜单里也能改，改了立即生效）</div>
      <label class="toggle"><input type="checkbox" data-toggle="running" ${S.paused ? '' : 'checked'}>
        <span>运行服务<br><span class="muted">取消后程序留在后台，但不分享视频、不同步批注，同事那边显示你离线</span></span></label>
      <label class="toggle"><input type="checkbox" data-toggle="autostart" ${S.toggles?.autostart ? 'checked' : ''} ${S.toggles?.autostartSupported ? '' : 'disabled'}>
        <span>开机启动</span></label>
      <label class="toggle"><input type="checkbox" data-toggle="silentStart" ${S.toggles?.silentStart ? 'checked' : ''} ${S.toggles?.autostart ? '' : 'disabled'}>
        <span>开机后静默运行<br><span class="muted">开机启动时不弹出浏览器，只在托盘里待命</span></span></label>
    </div>
    <div class="section row"><span class="muted">退出后，同事就看不了你的视频了。</span><span class="spacer"></span><button class="danger" id="su-quit">退出程序</button></div>`}
    <div class="modal-actions"><span class="error" id="su-err"></span>
      ${first ? '' : '<button id="su-cancel">取消</button>'}<button class="primary" id="su-ok">${first ? '开始使用' : '保存'}</button></div>`);
  async function browse(p) {
    try {
      const r = await api(`/api/fs?path=${encodeURIComponent(p || '')}`);
      path = r.path;
      roots = r.roots;
      $('#su-err').textContent = '';
      $('#su-path').textContent = `当前选中：${r.path}`;
      $('#su-roots').innerHTML = r.roots.map((x) => `<button data-p="${esc(x.path)}">${esc(x.label)}</button>`).join('');
      $('#su-dirs').innerHTML = (r.parent ? `<div class="dir" data-p="${esc(r.parent)}">⬆ 上一级</div>` : '')
        + (r.dirs.map((d) => `<div class="dir" data-p="${esc(d.path)}">📁 ${esc(d.name)}</div>`).join('') || '<div class="dir muted">（没有子文件夹）</div>');
    } catch (e) {
      $('#su-err').textContent = e.message;
    }
  }
  $('#modal-box').onclick = (e) => {
    const t = e.target.closest('[data-p]');
    if (t) browse(t.dataset.p);
  };
  $('#su-cancel')?.addEventListener('click', closeModal);
  $('#su-update')?.addEventListener('click', updateAction);
  renderUpdateSettings();
  $('#su-import').addEventListener('change', async (e) => {
    const file = e.target.files[0];
    e.target.value = '';
    if (!file) return;
    try {
      const data = JSON.parse(await file.text());
      const who = data.nickname ? `「${data.nickname}」` : '文件里';
      if (!confirm(`导入后，这台电脑会使用${who}的身份写批注。不要在两台电脑上同时使用同一个身份，导入后请不要再打开旧电脑上的程序。继续吗？`)) return;
      const r = await post('/api/identity', data);
      $('#su-code').textContent = r.code;
      if (data.nickname && !$('#su-name').value.trim()) $('#su-name').value = data.nickname;
      $('#su-err').textContent = '';
      alert(`已导入身份，设备码 ${r.code}`);
    } catch (err) {
      $('#su-err').textContent = err instanceof SyntaxError ? '不是有效的身份文件' : err.message;
    }
  });
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
  $('#su-quit')?.addEventListener('click', async () => {
    if (!confirm('退出程序后，同事就看不了你的视频了。确定退出吗？')) return;
    try { await post('/api/quit'); } catch { /* 程序已经在退出 */ }
    document.body.insertAdjacentHTML('beforeend', '<div id="quit-screen"><div><h2>程序已退出</h2><p class="muted">可以关闭这个页面。再次双击程序就能重新打开。</p></div></div>');
  });
  $('#su-ok').addEventListener('click', async () => {
    const nickname = $('#su-name').value.trim();
    if (!nickname) { $('#su-err').textContent = '先填一个昵称'; return; }
    if (roots.some((x) => x.path === path) && !confirm('共享整个个人文件夹或整个磁盘，会扫描里面所有视频。建议新建一个专门的文件夹。仍然继续吗？')) return;
    try {
      await post('/api/setup', { nickname, shareDir: path, teamKey: $('#su-key').value.trim() });
      closeModal();
      lastListKey = '';
      await refresh();
      if (first) openHelp();
    } catch (e) {
      $('#su-err').textContent = e.message;
    }
  });
  browse(path);
}

function openAddPeer() {
  openModal('peer', `
    <h2>添加电脑</h2>
    <p class="muted">同一个局域网里打开了程序的电脑一般会自动出现。如果过了一会儿还没出现（比如不在同一个网段，或者公司网络屏蔽了自动发现），在这里输入对方的 IP 地址。</p>
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
      <li>批注存在每个人共享文件夹里的"批注同步(别删)"文件夹，电脑之间自动同步，请不要手动删改。</li>
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

$('#btn-add-peer').onclick = () => S?.configured && openAddPeer();
$('#btn-help').onclick = () => S?.configured && openHelp();
$('#btn-settings').onclick = () => S?.configured && openSetup(false);

refresh();
setInterval(refresh, 2000);
