package main

// login-zcode 子命令：为 ZCode Coding Plan 渠道执行设备码登录，把兑换并
// 验证过的 Coding Plan key 写入 ~/.nanorouter/zcode_token.json，供网关的
// zcode_token_file 回退读取。密钥不打印到任何输出流。

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/nanorouter/nanorouter/internal/router"
)

const loginZcodeUsage = `usage: nanorouter login-zcode [-channel bigmodel|zai] [-gateway URL] [-timeout 5m] [-no-browser]

Runs the ZCode Coding Plan device authorization flow: opens the provider's
approval page, polls the gateway until the OAuth token lands, exchanges it
for the servable id.secret key, validates the key against the gateway, and
stores it in ~/.nanorouter/zcode_token.json (read back by candidates with
zcode_token_file: true).
`

// runZcodeLoginCommand 解析 login-zcode 的旗标并执行登录链。
// 返回进程退出码。
func runZcodeLoginCommand(args []string) int {
	fs := flag.NewFlagSet("login-zcode", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	channel := fs.String("channel", router.ZcodeChannelBigmodel, "Coding Plan channel: bigmodel or zai")
	gateway := fs.String("gateway", router.ZcodeDefaultOrigin, "gateway origin for device polling and probing")
	timeout := fs.Duration("timeout", router.ZcodeLoginTimeout, "how long to wait for approval")
	noBrowser := fs.Bool("no-browser", false, "do not attempt to open the authorization page automatically")
	if err := fs.Parse(args); err != nil {
		fmt.Fprint(os.Stderr, loginZcodeUsage)
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument %q\n", fs.Arg(0))
		fmt.Fprint(os.Stderr, loginZcodeUsage)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout+time.Minute)
	defer cancel()
	opts := router.ZcodeLoginOptions{
		GatewayOrigin: *gateway,
		Channel:       *channel,
		PollTimeout:   *timeout,
		Output:        os.Stderr,
	}
	if !*noBrowser {
		opts.OpenBrowser = openBrowser
	}
	path, err := router.RunZcodeLogin(ctx, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "login-zcode: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "login-zcode: configure the candidate with zcode_token_file: true and zcode_channel: %s\n", *channel)
	fmt.Fprintf(os.Stderr, "login-zcode: credential file: %s\n", path)
	return 0
}

// openBrowser 尽力而为地打开系统浏览器；失败由调用方提示手动打开。
func openBrowser(rawURL string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", rawURL).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL).Start()
	default:
		return exec.Command("xdg-open", rawURL).Start()
	}
}
