package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	stdlog "log"
	"os"
	"os/signal"
	"syscall"

	"github.com/github/github-mcp-server/pkg/github"
	iolog "github.com/github/github-mcp-server/pkg/log"
	"github.com/github/github-mcp-server/pkg/translations"
	gogithub "github.com/google/go-github/v69/github"
	"github.com/mark3labs/mcp-go/server"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var version = "version"
var commit = "commit"
var date = "date"

var (
	rootCmd = &cobra.Command{
		Use:     "server",
		Short:   "GitHub MCP Server",
		Long:    `A GitHub MCP server that handles various tools and resources.`,
		Version: fmt.Sprintf("%s (%s) %s", version, commit, date),
	}

	stdioCmd = &cobra.Command{
		Use:   "stdio",
		Short: "Start stdio server",
		Long:  `Start a server that communicates via standard input/output streams using JSON-RPC messages.`,
		Run: func(_ *cobra.Command, _ []string) {
			logFile := viper.GetString("log-file")
			readOnly := viper.GetBool("read-only")
			exportTranslations := viper.GetBool("export-translations")
			prettyPrintJSON := viper.GetBool("pretty-print-json")
			logger, err := initLogger(logFile)
			if err != nil {
				stdlog.Fatal("Failed to initialize logger:", err)
			}
			logCommands := viper.GetBool("enable-command-logging")
			cfg := runConfig{
				readOnly:           readOnly,
				logger:             logger,
				logCommands:        logCommands,
				exportTranslations: exportTranslations,
				prettyPrintJSON:    prettyPrintJSON,
			}
			if err := runStdioServer(cfg); err != nil {
				stdlog.Fatal("failed to run stdio server:", err)
			}
		},
	}
)

func init() {
	cobra.OnInitialize(initConfig)

	// Add global flags that will be shared by all commands
	rootCmd.PersistentFlags().Bool("read-only", false, "Restrict the server to read-only operations")
	rootCmd.PersistentFlags().String("log-file", "", "Path to log file")
	rootCmd.PersistentFlags().Bool("enable-command-logging", false, "When enabled, the server will log all command requests and responses to the log file")
	rootCmd.PersistentFlags().Bool("export-translations", false, "Save translations to a JSON file")
	rootCmd.PersistentFlags().String("gh-host", "", "Specify the GitHub hostname (for GitHub Enterprise etc.)")
	rootCmd.PersistentFlags().Bool("pretty-print-json", false, "Pretty print JSON output")

	// Bind flag to viper
	_ = viper.BindPFlag("read-only", rootCmd.PersistentFlags().Lookup("read-only"))
	_ = viper.BindPFlag("log-file", rootCmd.PersistentFlags().Lookup("log-file"))
	_ = viper.BindPFlag("enable-command-logging", rootCmd.PersistentFlags().Lookup("enable-command-logging"))
	_ = viper.BindPFlag("export-translations", rootCmd.PersistentFlags().Lookup("export-translations"))
	_ = viper.BindPFlag("gh-host", rootCmd.PersistentFlags().Lookup("gh-host"))
	_ = viper.BindPFlag("pretty-print-json", rootCmd.PersistentFlags().Lookup("pretty-print-json"))

	// Add subcommands
	rootCmd.AddCommand(stdioCmd)
}

func initConfig() {
	// Initialize Viper configuration
	viper.SetEnvPrefix("APP")
	viper.AutomaticEnv()
}

func initLogger(outPath string) (*log.Logger, error) {
	if outPath == "" {
		return log.New(), nil
	}

	file, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file: %w", err)
	}

	logger := log.New()
	logger.SetLevel(log.DebugLevel)
	logger.SetOutput(file)

	return logger, nil
}

type runConfig struct {
	readOnly           bool
	logger             *log.Logger
	logCommands        bool
	exportTranslations bool
	prettyPrintJSON    bool
}

// JSONPrettyPrintWriter is a Writer that pretty prints input to indented JSON
type JSONPrettyPrintWriter struct {
	writer io.Writer
}

func (j JSONPrettyPrintWriter) Write(p []byte) (n int, err error) {
	var prettyJSON bytes.Buffer
	if err := json.Indent(&prettyJSON, p, "", "\t"); err != nil {
		return 0, err
	}
	return j.writer.Write(prettyJSON.Bytes())
}

func runStdioServer(cfg runConfig) error {
	// Create app context
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Create GH client
	token := os.Getenv("GITHUB_PERSONAL_ACCESS_TOKEN")
	if token == "" {
		cfg.logger.Fatal("GITHUB_PERSONAL_ACCESS_TOKEN not set")
	}
	ghClient := gogithub.NewClient(nil).WithAuthToken(token)
	ghClient.UserAgent = fmt.Sprintf("github-mcp-server/%s", version)

	// Check GH_HOST env var first, then fall back to viper config
	host := os.Getenv("GH_HOST")
	if host == "" {
		host = viper.GetString("gh-host")
	}

	if host != "" {
		var err error
		ghClient, err = ghClient.WithEnterpriseURLs(host, host)
		if err != nil {
			return fmt.Errorf("failed to create GitHub client with host: %w", err)
		}
	}

	t, dumpTranslations := translations.TranslationHelper()

	// Create
	ghServer := github.NewServer(ghClient, version, cfg.readOnly, t)
	stdioServer := server.NewStdioServer(ghServer)

	stdLogger := stdlog.New(cfg.logger.Writer(), "stdioserver", 0)
	stdioServer.SetErrorLogger(stdLogger)

	if cfg.exportTranslations {
		// Once server is initialized, all translations are loaded
		dumpTranslations()
	}

	// Start listening for messages
	errC := make(chan error, 1)
	go func() {
		in, out := io.Reader(os.Stdin), io.Writer(os.Stdout)

		if cfg.logCommands {
			loggedIO := iolog.NewIOLogger(in, out, cfg.logger)
			in, out = loggedIO, loggedIO
		}

		if cfg.prettyPrintJSON {
			out = JSONPrettyPrintWriter{writer: out}
		}
		errC <- stdioServer.Listen(ctx, in, out)
	}()

	// Output github-mcp-server string
	_, _ = fmt.Fprintf(os.Stderr, "GitHub MCP Server running on stdio\n")

	// Wait for shutdown signal
	select {
	case <-ctx.Done():
		cfg.logger.Infof("shutting down server...")
	case err := <-errC:
		if err != nil {
			return fmt.Errorf("error running server: %w", err)
		}
	}

	return nil
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
