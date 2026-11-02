package main

// login zcode 子命令：为 ZCode Coding Plan 渠道执行设备码登录，把兑换并
// 验证过的 Coding Plan key 写入 ~/.nanorouter/zcode_token.json，供网关的
// zcode_token_file 回退读取。密钥不打印到任何输出流。

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/nanorouter/nanorouter/internal/router"
)

// newLoginZcodeCommand 组装 login zcode 子命令，旗标与旧 flag 版本一致。
func newLoginZcodeCommand() *cobra.Command {
	var (
		channel   string
		gateway   string
		timeout   time.Duration
		noBrowser bool
	)
	cmd := &cobra.Command{
		Use:   "zcode",
		Short: "Run the ZCode Coding Plan device authorization flow and store the credential",
		Long: `Runs the ZCode Coding Plan device authorization flow: opens the provider's
approval page, polls the gateway until the OAuth token lands, exchanges it
for the servable id.secret key, validates the key against the gateway, and
stores it in ~/.nanorouter/zcode_token.json (read back by candidates with
zcode_token_file: true).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), timeout+time.Minute)
			defer cancel()
			opts := router.ZcodeLoginOptions{
				GatewayOrigin: gateway,
				Channel:       channel,
				PollTimeout:   timeout,
				Output:        cmd.ErrOrStderr(),
			}
			if !noBrowser {
				opts.OpenBrowser = openBrowser
			}
			path, err := router.RunZcodeLogin(ctx, opts)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "login zcode: configure the candidate with zcode_token_file: true and zcode_channel: %s\n", channel)
			fmt.Fprintf(cmd.ErrOrStderr(), "login zcode: credential file: %s\n", path)
			return nil
		},
	}
	cmd.Flags().StringVar(&channel, "channel", router.ZcodeChannelBigmodel, "Coding Plan channel: bigmodel or zai")
	cmd.Flags().StringVar(&gateway, "gateway", router.ZcodeDefaultOrigin, "gateway origin for device polling and probing")
	cmd.Flags().DurationVar(&timeout, "timeout", router.ZcodeLoginTimeout, "how long to wait for approval")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "do not attempt to open the authorization page automatically")
	return cmd
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
