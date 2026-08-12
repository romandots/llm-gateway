package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/romandots/llm-gateway/internal/config"
	"github.com/romandots/llm-gateway/internal/litellm"
	"github.com/romandots/llm-gateway/internal/report"
	"github.com/spf13/cobra"
)

// version is stamped at build time: go build -ldflags "-X main.version=v1.0.0".
var version = "dev"

// globals holds the flags shared by every command.
type globals struct {
	root      string
	configDir string
	endpoint  string
	masterKey string
	output    string
	timeout   time.Duration
	assumeYes bool
}

var opts = &globals{}

func newRootCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "gwctl",
		Short:         "Control plane for the LLM gateway",
		SilenceUsage:  true,
		SilenceErrors: true,
		Long: strings.TrimSpace(`
gwctl reconciles the declarative configuration in config/*.yaml with a running
LiteLLM proxy, and manages consumer keys, spend reports and health checks.

Configuration is the source of truth; the proxy is brought to it. Secrets are
never read from or written to the configuration: vendor keys live in the proxy
container's environment, and consumer keys are printed exactly once, at issue.`),
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if !report.ValidFormat(opts.output) {
				return fmt.Errorf("unknown --output %q, expected %s or %s", opts.output, report.FormatTable, report.FormatJSON)
			}
			if !cmd.Flags().Changed("config") {
				opts.configDir = filepath.Join(opts.root, "config")
			}
			loadEnvFile(filepath.Join(opts.root, "deploy", ".env"))
			if opts.masterKey == "" {
				opts.masterKey = os.Getenv("LITELLM_MASTER_KEY")
			}
			if endpoint := os.Getenv("GATEWAY_ENDPOINT"); endpoint != "" && !cmd.Flags().Changed("endpoint") {
				opts.endpoint = endpoint
			}
			return nil
		},
	}

	flags := cmd.PersistentFlags()
	flags.StringVar(&opts.root, "root", ".", "repository root holding config/ and deploy/")
	flags.StringVarP(&opts.configDir, "config", "c", "config", "directory with models.yaml, consumers.yaml, proxy.yaml")
	flags.StringVar(&opts.endpoint, "endpoint", "http://localhost:4000", "LiteLLM proxy base URL (env GATEWAY_ENDPOINT)")
	flags.StringVar(&opts.masterKey, "master-key", "", "LiteLLM master key (env LITELLM_MASTER_KEY, or deploy/.env)")
	flags.StringVarP(&opts.output, "output", "o", report.FormatTable, "output format: table|json")
	flags.DurationVar(&opts.timeout, "timeout", 60*time.Second, "timeout for calls to the proxy")
	flags.BoolVarP(&opts.assumeYes, "yes", "y", false, "do not ask for confirmation")

	cmd.AddCommand(
		newValidateCommand(),
		newApplyCommand(),
		newDiffCommand(),
		newKeyCommand(),
		newSpendCommand(),
		newModelsCommand(),
		newHealthCommand(),
		newVersionCommand(),
	)
	return cmd
}

// loadConfig parses and validates the configuration. Commands that talk to the
// proxy refuse to run on a configuration that does not validate: pushing a
// half-broken state to a live gateway is worse than stopping.
func loadConfig() (*config.Config, error) {
	cfg, err := config.Load(opts.configDir)
	if err != nil {
		return nil, err
	}
	if issues := cfg.Validate(); issues.HasErrors() {
		for _, issue := range issues {
			if issue.Severity == config.SeverityError {
				fmt.Fprintln(os.Stderr, issue)
			}
		}
		return nil, fmt.Errorf("configuration is invalid; run `gwctl validate` for the full report")
	}
	return cfg, nil
}

func newClient() (*litellm.Client, error) {
	if opts.masterKey == "" {
		return nil, fmt.Errorf("no master key: pass --master-key, set LITELLM_MASTER_KEY, or put it in deploy/.env")
	}
	return litellm.New(opts.endpoint, opts.masterKey, opts.timeout), nil
}

// confirm asks before a change that is hard to undo. --yes skips it, which is
// what non-interactive callers (make targets, CI) use.
func confirm(prompt string) error {
	if opts.assumeYes {
		return nil
	}
	fmt.Printf("%s [y/N]: ", prompt)
	reader := bufio.NewReader(os.Stdin)
	answer, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read confirmation: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return nil
	default:
		return fmt.Errorf("aborted")
	}
}

// loadEnvFile makes `gwctl` usable on the VPS without exporting anything by
// hand. Values already present in the environment win, and nothing is ever
// written back.
func loadEnvFile(path string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		name, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		name = strings.TrimSpace(name)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if _, exists := os.LookupEnv(name); !exists {
			_ = os.Setenv(name, value)
		}
	}
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the gwctl version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return report.Emit(cmd.OutOrStdout(), opts.output,
				report.Table{Headers: []string{"component", "version"}, Rows: [][]string{{"gwctl", version}}},
				map[string]string{"gwctl": version})
		},
	}
}
