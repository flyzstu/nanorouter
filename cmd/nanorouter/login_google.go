// login google 子命令：为 Google Antigravity 执行 OAuth 登录，把获取的
// access token 和 refresh token 写入 ~/.nanorouter/google_token.json。

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/nanorouter/nanorouter/internal/router"
)

// newLoginGoogleCommand 组装 login google 子命令。
func newLoginGoogleCommand() *cobra.Command {
	var (
		scope       string
		port        int
		timeout     time.Duration
		noBrowser   bool
		redirectURL string
	)
	cmd := &cobra.Command{
		Use:   "google",
		Short: "Log in to Google Antigravity via OAuth",
		Long: `Log in to Google Antigravity (Gemini, Claude, GPT-OSS) via OAuth.

On a machine with a browser:
  nanorouter login google
  The browser opens automatically. After authorization, the token is saved.

On a headless/remote server:
  nanorouter login google
  Copy the printed URL, open it in a browser on your local machine.
  After authorization, the browser redirects to a URL like:
    http://127.0.0.1:51121/oauth-callback?code=...
  The page will show an error (expected on remote), but copy the full URL
  from the address bar and paste it back at the terminal prompt.

Token is saved to ~/.nanorouter/google_token.json.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), timeout+time.Minute)
			defer cancel()

			opts := router.GoogleLoginOptions{
				Scope:        scope,
				CallbackPort: port,
				PollTimeout:  timeout,
				Output:       cmd.ErrOrStderr(),
				RedirectURL:  redirectURL,
			}
			if !noBrowser {
				opts.OpenBrowser = openBrowser
			}

			path, err := router.RunGoogleLogin(ctx, opts)
			if err != nil {
				return err
			}

			fmt.Fprintf(cmd.ErrOrStderr(), "login google: credential file: %s\n", path)
			return nil
		},
	}

	cmd.Flags().StringVar(&scope, "scope", "", "OAuth scope (default: Antigravity scopes)")
	cmd.Flags().IntVar(&port, "port", router.GoogleCallbackDefaultPort, "callback server port")
	cmd.Flags().DurationVar(&timeout, "timeout", router.GoogleLoginTimeout, "timeout for authorization")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "do not open the browser automatically")
	cmd.Flags().StringVar(&redirectURL, "redirect-url", "", "provide the redirect URL directly (skip interactive flow)")

	return cmd
}
