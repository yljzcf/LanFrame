// Package store 放批注数据：签名的批注记录（每条一个文件，只增不改）、设备身份，
// 以及共享文件夹里的三个子文件夹（见 layout.go）。
package store

// appName 写在导出的身份文件里，用来认出是不是本程序的文件。
const appName = "lan-video-review"

// ValidRecordID 判断是不是合法的记录 ID（也用于批注 ID）。
func ValidRecordID(s string) bool { return reRecordID.MatchString(s) }

// ValidImageName 判断是不是合法的图片文件名（内容哈希 + 扩展名）。
func ValidImageName(s string) bool { return reImageName.MatchString(s) }

// Dir 是"批注"文件夹的路径（还没改名的话是旧的"批注同步(别删)"）。
func (s *Store) Dir() string { return s.dir }
