// Package media 放视频相关的部分：扫描共享文件夹、算文件指纹、读取 MP4 帧表，以及版本组和改名。
package media

// ValidFP 判断是不是合法的视频指纹。
func ValidFP(s string) bool { return reFP.MatchString(s) }

// Path 是视频文件在本机上的完整路径（不会发给同事）。
func (v *VideoInfo) Path() string { return v.path }
