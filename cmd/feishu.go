package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/opencode-ai/opencode/internal/app"
	"github.com/opencode-ai/opencode/internal/config"
	"github.com/opencode-ai/opencode/internal/db"
	"github.com/opencode-ai/opencode/internal/feishu"
	"github.com/opencode-ai/opencode/internal/logging"
	"github.com/spf13/cobra"
)

var feishuCmd = &cobra.Command{
	Use:   "feishu",
	Short: "Run OpenCode as a Feishu (Lark) bot",
	Long: `Start OpenCode as a Feishu bot. Messages received from Feishu are
forwarded to the coding agent and the reply is sent back to the chat.

Environment variables:
  FEISHU_APP_ID         Feishu app ID
  FEISHU_APP_SECRET     Feishu app secret
  FEISHU_VERIFICATION_TOKEN  Event subscription verification token
  FEISHU_LISTEN_ADDR    HTTP listen address (default ":8080")
  FEISHU_PUBLIC_URL     Public base URL for callback (logging hint)`,
	RunE: func(cmd *cobra.Command, args []string) error {
		debug, _ := cmd.Flags().GetBool("debug")
		cwd, _ := cmd.Flags().GetString("cwd")
		addr, _ := cmd.Flags().GetString("addr")

		if cwd == "" {
			c, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("failed to get working directory: %w", err)
			}
			cwd = c
		}
		_ = os.Chdir(cwd)

		if _, err := config.Load(cwd, debug); err != nil {
			return err
		}

		conn, err := db.Connect()
		if err != nil {
			return err
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		application, err := app.New(ctx, conn)
		if err != nil {
			return err
		}
		defer application.Shutdown()

		initMCPTools(ctx, application)

		cfg := feishu.Config{
			AppID:            getEnv("FEISHU_APP_ID", ""),
			AppSecret:        getEnv("FEISHU_APP_SECRET", ""),
			VerificationToken: getEnv("FEISHU_VERIFICATION_TOKEN", ""),
			EncryptKey:       getEnv("FEISHU_ENCRYPT_KEY", ""),
			ListenAddr:       addr,
			PublicBaseURL:    getEnv("FEISHU_PUBLIC_URL", ""),
		}

		if cfg.AppID == "" || cfg.AppSecret == "" {
			return fmt.Errorf("FEISHU_APP_ID and FEISHU_APP_SECRET must be set")
		}
		if cfg.ListenAddr == "" {
			cfg.ListenAddr = ":8080"
		}

		handler := feishu.NewHandler(cfg)
		handler.MessageFunc = func(ctx context.Context, msg feishu.Message) (string, error) {
			// Strip the bot mention prefix if present (group chats).
			prompt := strings.TrimSpace(strings.Replace(msg.Content, "@_user_1", "", -1))
			if prompt == "" {
				return "你好，我是 OpenCode 机器人，把你的编程问题发给我吧～", nil
			}

			// Reuse the non-interactive flow: prompt -> agent -> text.
			var reply strings.Builder
			replyDone := make(chan struct{})
			go func() {
				defer close(replyDone)
				_ = application.RunNonInteractiveCapture(ctx, prompt, &reply)
			}()
			select {
			case <-replyDone:
			case <-time.After(5 * time.Minute):
				return "处理超时了，请稍后再试。", nil
			}
			return strings.TrimSpace(reply.String()), nil
		}

		logging.Info("Starting OpenCode Feishu bot", "addr", cfg.ListenAddr)
		return handler.Serve(ctx)
	},
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func init() {
	feishuCmd.Flags().BoolP("debug", "d", false, "Debug mode")
	feishuCmd.Flags().StringP("cwd", "c", "", "Working directory")
	feishuCmd.Flags().StringP("addr", "a", ":8080", "HTTP listen address for Feishu callback")
	rootCmd.AddCommand(feishuCmd)
}
