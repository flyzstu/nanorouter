// login mistral 子命令：执行 Mistral Vibe 浏览器登录，把平台 API key
// 写入 ~/.nanorouter/mistral_token.json（0600）。流程对齐 mistral-vibe
// 客户端：PKCE sign-in → 浏览器授权 → 轮询 → exchange 换取 API key。

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/nanorouter/nanorouter/internal/router"
)

// newLoginMistralCommand 组装 login mistral 子命令。
func newLoginMistralCommand() *cobra.Command {
	var (
		browserURL string
		apiURL     string
		timeout    time.Duration
		noBrowser  bool
	)
	cmd := &cobra.Command{
		Use:   "mistral",
		Short: "Log in to Mistral Vibe via browser sign-in",
		Long: `Log in to Mistral Vibe via browser sign-in (PKCE).

On a machine with a browser:
  nanorouter login mistral
  The browser opens automatically. After authorization, the API key is saved.

On a headless/remote server:
  nanorouter login mistral
  Copy the printed sign-in URL, open it in a browser on your local machine,
  and authorize. The command polls until you finish, then exchanges the
  approval for an API key and saves it.

The key is saved to ~/.nanorouter/mistral_token.json. Configure the route
candidate with mistral_token_file: true and base_url https://api.mistral.ai.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), timeout+time.Minute)
			defer cancel()

			opts := router.MistralLoginOptions{
				BrowserBaseURL: browserURL,
				APIBaseURL:     apiURL,
				PollTimeout:    timeout,
				Output:         cmd.ErrOrStderr(),
			}
			if !noBrowser {
				opts.OpenBrowser = openBrowser
			}

			path, err := router.RunMistralLogin(ctx, opts)
			if err != nil {
				return err
			}

			fmt.Fprintf(cmd.ErrOrStderr(), "login mistral: credential file: %s\n", path)
			return nil
		},
	}

	cmd.Flags().StringVar(&browserURL, "browser-url", router.MistralDefaultBrowserURL, "console base URL (sign-in page origin)")
	cmd.Flags().StringVar(&apiURL, "api-url", router.MistralDefaultAPIBaseURL, "console API base URL (sign-in/exchange endpoint)")
	cmd.Flags().DurationVar(&timeout, "timeout", router.MistralLoginTimeout, "timeout for authorization")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "do not open the browser automatically")

	return cmd
}
