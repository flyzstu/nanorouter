//go:build windows

package router

// zcodeOSVersion 在 Windows 上尽力而为地返回版本号。node os.release() 在
// win32 返回 "10.0.xxxxx" 形式；不引入 x/sys 依赖时无法可靠区分具体构建，
// 返回 "10"（Windows 10/11 的共同主版本），注释为最佳努力指纹。
func zcodeOSVersion() string {
	return "10"
}
