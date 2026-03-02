package plugincmd

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"plugin"
	"runtime"
	"strconv"
	"strings"

	"github.com/paccolamano/plugin/pbplugin"
	"github.com/paccolamano/plugin/plugincmd/internal/git"
	"github.com/paccolamano/plugin/plugincmd/internal/util"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/security"
	"github.com/spf13/cobra"
)

const (
	pluginCollectionName = "_plugins"
	defaultPluginFolder  = "pb_plugins"
)

var (
	ErrAlreadyInstalled = errors.New("already installed")
	ErrNotInstalled     = errors.New("not installed")
)

type Config struct {
	Dir         string
	Autorestart bool
}

func MustRegister(app core.App, rootCmd *cobra.Command, config Config) {
	if err := Register(app, rootCmd, config); err != nil {
		panic(err)
	}
}

func Register(app core.App, rootCmd *cobra.Command, config Config) error {
	if config.Dir == "" {
		config.Dir = filepath.Join(app.DataDir(), "..", defaultPluginFolder)
	}

	pm := &pluginCmd{app: app, config: config}

	app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		if err := ensureCollection(e.App); err != nil {
			return err
		}

		if pm.config.Autorestart && util.IsServeProcess() {
			if err := os.MkdirAll(pm.config.Dir, 0o755); err == nil {
				_ = os.WriteFile(util.PidFilePath(pm.config.Dir), []byte(strconv.Itoa(os.Getpid())), 0o644)
			}
			util.SetupRestartSignal()
		}

		return loadAll(e.App, pm.config.Dir)
	})

	if rootCmd != nil {
		rootCmd.AddCommand(pm.newCommand())
	}

	return nil
}

func ensureCollection(app core.App) error {
	if _, err := app.FindCollectionByNameOrId(pluginCollectionName); err == nil {
		return nil // already exists
	}

	pluginCollection := core.NewBaseCollection(pluginCollectionName)
	pluginCollection.System = true

	pluginCollection.Fields.Add(
		&core.TextField{
			Name:     "pluginUri",
			Required: true,
		},
		&core.TextField{
			Name:     "buildFile",
			Required: true,
		},
		&core.TextField{
			Name:     "version",
			Required: true,
		},
		&core.AutodateField{
			Name:     "installed",
			OnCreate: true,
			OnUpdate: false,
		},
	)

	return app.Save(pluginCollection)
}

func loadAll(app core.App, dir string) error {
	records, err := app.FindAllRecords(pluginCollectionName)
	if err != nil {
		return nil // collection does not exist yet
	}

	for _, record := range records {
		uri := record.GetString("pluginUri")
		soPath := filepath.Join(dir, record.GetString("buildFile"))

		p, err := plugin.Open(soPath)
		if err != nil {
			return fmt.Errorf("plugin %q: open failed: %w", uri, err)
		}

		sym, err := p.Lookup("Plugin")
		if err != nil {
			return fmt.Errorf("plugin %q: missing exported 'Plugin' symbol: %w", uri, err)
		}

		pbPlugin, ok := sym.(*pbplugin.PBPlugin)
		if !ok {
			return fmt.Errorf("plugin %q: 'Plugin' does not implement PBPlugin", uri)
		}

		if err := (*pbPlugin).Register(app); err != nil {
			return fmt.Errorf("plugin %q: Register failed: %w", uri, err)
		}
	}

	return nil
}

type pluginCmd struct {
	app    core.App
	config Config
}

func (pm *pluginCmd) newCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "plugin",
		Short:        "Manage PocketBase plugins",
		SilenceUsage: true,
	}

	cmd.AddCommand(pm.cmdInstall())
	cmd.AddCommand(pm.cmdRemove())
	cmd.AddCommand(pm.cmdList())

	return cmd
}

func (pm *pluginCmd) cmdInstall() *cobra.Command {
	var token, provider string

	cmd := &cobra.Command{
		Use:          "install <owner/repo | https://... | ./path> [version]",
		Short:        "Install a plugin from GitHub, a Git hosting provider, or a local path",
		Args:         cobra.RangeArgs(1, 2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			target := args[0]
			version := "latest"
			if len(args) == 2 {
				version = args[1]
			}

			if util.IsLocalPath(target) && len(args) == 2 {
				return fmt.Errorf("version argument is not supported for local plugins")
			}
			if strings.HasPrefix(target, "https://") && !cmd.Flags().Changed("provider") {
				return fmt.Errorf("flag --provider is required when target is a URL (supported: github, gitea, forgejo, gitlab)")
			}

			err := pm.install(cmd.Context(), target, version, provider, token)
			if errors.Is(err, ErrAlreadyInstalled) {
				return nil
			}
			return err
		},
	}

	cmd.Flags().StringVar(&provider, "provider", "", "git provider for URL targets: github, gitea, forgejo, gitlab")
	cmd.Flags().StringVar(&token, "token", "", "personal access token (required for private repositories)")

	return cmd
}

func (pm *pluginCmd) install(ctx context.Context, target, version, provider, token string) error {
	if util.IsLocalPath(target) {
		return pm.installLocal(target)
	}

	if strings.HasPrefix(target, "https://") {
		return pm.installFromURL(ctx, target, version, provider, token)
	}

	// owner/repo = shorthand github
	if strings.Count(target, "/") == 1 && !strings.HasPrefix(target, "/") {
		return pm.installFromURL(ctx, "https://github.com/"+target, version, "github", token)
	}

	return fmt.Errorf("invalid target %q: expected owner/repo, https://..., or a local path (./...)", target)
}

func (pm *pluginCmd) installFromURL(ctx context.Context, repoURL, version, provider, token string) (err error) {
	u, err := url.Parse(repoURL)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", repoURL, err)
	}

	webBase := u.Scheme + "://" + u.Host
	parts := strings.SplitN(strings.TrimPrefix(u.Path, "/"), "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("cannot extract owner/repo from URL %q", repoURL)
	}

	ownerRepo := parts[0] + "/" + parts[1]
	repoName := parts[1]

	if existing, err := pm.app.FindFirstRecordByData(pluginCollectionName, "pluginUri", repoURL); err == nil {
		return fmt.Errorf("plugin %q is already installed (version %s): %w", repoURL, existing.GetString("version"), ErrAlreadyInstalled)
	}

	apiBase := git.APIBaseURL(provider, webBase)
	gc, err := git.NewClient(provider, apiBase, token)
	if err != nil {
		return err
	}

	release, err := gc.GetRelease(ctx, ownerRepo, version)
	if err != nil {
		return err
	}

	tmpDir, err := os.MkdirTemp("", "pbplugin-*")
	if err != nil {
		return fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	fmt.Printf("Downloading %s@%s...\n", repoURL, release.TagName)
	body, err := gc.DownloadRelease(ctx, release.TarballURL)
	if err != nil {
		return err
	}
	defer body.Close()

	srcDir, err := util.ExtractTarball(body, tmpDir)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(pm.config.Dir, 0o755); err != nil {
		return fmt.Errorf("failed to create plugins directory: %w", err)
	}

	soName := fmt.Sprintf("%s_%s_%s_%s.so", security.RandomString(10), repoName, runtime.GOOS, runtime.GOARCH)
	destPath := filepath.Join(pm.config.Dir, soName)
	defer func() {
		if err != nil {
			os.Remove(destPath)
		}
	}()

	fmt.Printf("Compiling %s...\n", repoURL)
	if err := util.CompilePlugin(srcDir, destPath); err != nil {
		return err
	}

	pluginCollection, err := pm.app.FindCollectionByNameOrId(pluginCollectionName)
	if err != nil {
		return fmt.Errorf("plugins collection not found: %w", err)
	}

	record := core.NewRecord(pluginCollection)
	record.Set("pluginUri", repoURL)
	record.Set("buildFile", soName)
	record.Set("version", release.TagName)

	if err := pm.app.Save(record); err != nil {
		return fmt.Errorf("failed to save plugin record: %w", err)
	}

	fmt.Printf("Installed %s@%s\n", repoURL, release.TagName)
	pm.maybeRestart()
	return nil
}

func (pm *pluginCmd) installLocal(localPath string) (err error) {
	absPath, err := filepath.Abs(localPath)
	if err != nil {
		return fmt.Errorf("failed to resolve path: %w", err)
	}

	uri := "file://" + absPath

	if existing, err := pm.app.FindFirstRecordByData(pluginCollectionName, "pluginUri", uri); err == nil {
		return fmt.Errorf("plugin from %q is already installed (version %s): %w", absPath, existing.GetString("version"), ErrAlreadyInstalled)
	}

	if err := os.MkdirAll(pm.config.Dir, 0o755); err != nil {
		return fmt.Errorf("failed to create plugins directory: %w", err)
	}

	soName := fmt.Sprintf("%s_%s_%s_%s.so", security.RandomString(10), filepath.Base(absPath), runtime.GOOS, runtime.GOARCH)
	destPath := filepath.Join(pm.config.Dir, soName)
	defer func() {
		if err != nil {
			os.Remove(destPath)
		}
	}()

	fmt.Printf("Compiling %s...\n", absPath)
	if err := util.CompilePlugin(absPath, destPath); err != nil {
		return err
	}

	pluginCollection, err := pm.app.FindCollectionByNameOrId(pluginCollectionName)
	if err != nil {
		return fmt.Errorf("plugins collection not found: %w", err)
	}

	record := core.NewRecord(pluginCollection)
	record.Set("pluginUri", uri)
	record.Set("buildFile", soName)
	record.Set("version", "unknown")

	if err := pm.app.Save(record); err != nil {
		return fmt.Errorf("failed to save plugin record: %w", err)
	}

	fmt.Printf("Installed %s\n", absPath)
	pm.maybeRestart()
	return nil
}

func (pm *pluginCmd) cmdRemove() *cobra.Command {
	return &cobra.Command{
		Use:          "rm <owner/repo | https://... | ./path>",
		Short:        "Remove an installed plugin",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			err := pm.remove(args[0])
			if errors.Is(err, ErrNotInstalled) {
				return nil
			}
			return err
		},
	}
}

func (pm *pluginCmd) remove(target string) error {
	record, err := pm.findByURI(target)
	if err != nil {
		return fmt.Errorf("plugin %q is not installed: %w", target, ErrNotInstalled)
	}

	soPath := filepath.Join(pm.config.Dir, record.GetString("buildFile"))
	if err := os.Remove(soPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove plugin file: %w", err)
	}

	if err := pm.app.Delete(record); err != nil {
		return fmt.Errorf("failed to remove plugin record: %w", err)
	}

	fmt.Printf("Removed %s\n", target)
	pm.maybeRestart()
	return nil
}

func (pm *pluginCmd) findByURI(target string) (*core.Record, error) {
	uri, err := resolveURI(target)
	if err != nil {
		return nil, err
	}
	return pm.app.FindFirstRecordByData(pluginCollectionName, "pluginUri", uri)
}

func resolveURI(target string) (string, error) {
	if util.IsLocalPath(target) {
		absPath, err := filepath.Abs(target)
		if err != nil {
			return "", err
		}
		return "file://" + absPath, nil
	}
	if strings.HasPrefix(target, "https://") {
		return target, nil
	}
	// owner/repo shorthand → GitHub
	if strings.Count(target, "/") == 1 {
		return "https://github.com/" + target, nil
	}
	return "", fmt.Errorf("unrecognized target format %q", target)
}

func (pm *pluginCmd) cmdList() *cobra.Command {
	return &cobra.Command{
		Use:          "ls",
		Short:        "List installed plugins",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return pm.list()
		},
	}
}

func (pm *pluginCmd) list() error {
	records, err := pm.app.FindAllRecords(pluginCollectionName)
	if err != nil || len(records) == 0 {
		fmt.Println("No plugins installed.")
		return nil
	}

	fmt.Printf("%-55s %s\n", "PLUGIN", "VERSION")
	fmt.Println(strings.Repeat("-", 70))
	for _, r := range records {
		fmt.Printf("%-55s %s\n", r.GetString("pluginUri"), r.GetString("version"))
	}
	return nil
}

func (pm *pluginCmd) maybeRestart() {
	if pm.config.Autorestart {
		fmt.Println("Signaling server to restart...")
		if err := util.SignalServe(pm.config.Dir); err != nil {
			fmt.Printf("Warning: %v\nPlease restart the server manually.\n", err)
		}
	}
}
