//go:build !windows

package router

import "syscall"

// zcodeOSVersion 返回内核版本（对应参考客户端 node os.release() 的
// 语义：Linux 上是 uname -r，例如 7.1.13-200.fc44.x86_64）。uname 失败
// 时返回空串：该头只是指纹的一部分，缺失不致拒绝。
func zcodeOSVersion() string {
	var uts syscall.Utsname
	if err := syscall.Uname(&uts); err != nil {
		return ""
	}
	release := make([]byte, 0, len(uts.Release))
	for _, c := range uts.Release {
		if c == 0 {
			break
		}
		release = append(release, byte(c))
	}
	return string(release)
}
