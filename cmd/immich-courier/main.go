package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"

	"github.com/zalando/go-keyring"
	"golang.org/x/term"

	"immich-courier/internal/auth"
	"immich-courier/internal/config"
	"immich-courier/internal/daemon"
	"immich-courier/internal/immich"
	"immich-courier/internal/state"
)

var version = "dev" // set with -ldflags "-X main.version=..."

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		if !guiAvailable() {
			usage()
			os.Exit(2)
		}
		args = []string{"tray"} // double-click / start at login
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cmds := map[string]func(context.Context, []string) error{
		"tray":          cmdTray,
		"run":           cmdRun,
		"status":        cmdStatus,
		"pause":         ipcCmd("pause", (*daemon.Client).Pause, "Uploads paused. Files are still watched and queued."),
		"resume":        ipcCmd("resume", (*daemon.Client).Resume, "Uploads resumed."),
		"rescan":        ipcCmd("rescan", (*daemon.Client).Rescan, "Rescanning folders."),
		"stop":          ipcCmd("stop", (*daemon.Client).Quit, "Stopped."),
		"login":         cmdLogin,
		"logout":        cmdLogout,
		"add-folder":    cmdAddFolder,
		"remove-folder": cmdRemoveFolder,
		"list":          cmdList,
	}
	switch args[0] {
	case "-h", "--help", "help":
		usage()
		return
	case "-v", "--version", "version":
		fmt.Println("immich-courier", version)
		return
	}
	f, ok := cmds[args[0]]
	if !ok {
		usage()
		os.Exit(2)
	}
	if err := f(ctx, args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `immich-courier: watch folders and upload new photos/videos to Immich

Setup:
  login          --account NAME --server URL [--method auto|oidc|password|apikey]
  logout         --account NAME
  add-folder     --path DIR --account NAME
  remove-folder  --path DIR
  list           show accounts and folders

Running:
  tray           watch and upload with a system tray icon (the default with no arguments)
  run            watch and upload in the foreground, no tray (for servers)
  status         show what a running instance is doing
  pause|resume   pause or resume uploads (watching and queuing continue)
  rescan         rescan folders now and retry waiting uploads
  stop           stop a running instance

Every command takes --config FILE (default: `+defaultConfigPath()+`).
`)
}

func defaultConfigPath() string {
	p, err := config.DefaultPath()
	if err != nil {
		return "config.toml"
	}
	return p
}

func configFlag(fs *flag.FlagSet) *string {
	return fs.String("config", defaultConfigPath(), "config file")
}

func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := configFlag(fs)
	verbose := fs.Bool("v", false, "debug logging")
	fs.Parse(args)

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	d, err := daemon.Open(*cfgPath, version, log)
	if err != nil {
		return err
	}
	log.Info("immich-courier started", "version", version, "config", *cfgPath)
	err = d.Run(ctx)
	log.Info("immich-courier stopped")
	return err
}

// ipcCmd builds a command that sends one request to the running instance.
func ipcCmd(name string, call func(*daemon.Client, context.Context) error, done string) func(context.Context, []string) error {
	return func(ctx context.Context, args []string) error {
		fs := flag.NewFlagSet(name, flag.ExitOnError)
		cfgPath := configFlag(fs)
		fs.Parse(args)
		c, err := daemon.Dial(*cfgPath)
		if err != nil {
			return err
		}
		if err := call(c, ctx); err != nil {
			return err
		}
		fmt.Println(done)
		return nil
	}
}

// notifyReload tells a running instance to pick up config changes.
func notifyReload(ctx context.Context, cfgPath string) {
	c, err := daemon.Dial(cfgPath)
	if err != nil {
		fmt.Println("(immich-courier isn't running; the change applies when it starts)")
		return
	}
	if err := c.Reload(ctx); err != nil {
		fmt.Println("The running instance reloaded, but reports:", err)
		return
	}
	fmt.Println("The running instance picked up the change.")
}

func cmdStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	cfgPath := configFlag(fs)
	fs.Parse(args)
	c, err := daemon.Dial(*cfgPath)
	if err != nil {
		return err
	}
	s, err := c.Status(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("immich-courier %s, running since %s\n", s.Version, s.Started.Local().Format("2006-01-02 15:04"))
	if s.Paused {
		fmt.Println("Uploads are PAUSED (resume with: immich-courier resume)")
	}
	if s.Problem != "" {
		fmt.Println("Not uploading:", s.Problem)
	}
	for _, a := range s.Accounts {
		conn := "online"
		switch {
		case a.AuthFailed:
			conn = "API KEY REJECTED, log in again"
		case !a.Online:
			conn = "server unreachable, waiting"
		}
		fmt.Printf("\nAccount %q  %s  [%s]\n", a.Name, a.Server, conn)
		fmt.Printf("  waiting: %d   uploaded since start: %d", a.Queued, a.Uploaded)
		if !a.LastUpload.IsZero() {
			fmt.Printf("   last upload: %s", a.LastUpload.Local().Format("15:04:05"))
		}
		fmt.Println()
		if a.Uploading != "" {
			fmt.Println("  uploading:", a.Uploading)
		}
		if a.LastError != "" {
			fmt.Println("  last error:", a.LastError)
		}
		for _, f := range s.Folders {
			if f.Account == a.Name {
				fmt.Println("  watching", f.Path)
			}
		}
	}
	return nil
}

func cmdAddFolder(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("add-folder", flag.ExitOnError)
	cfgPath := configFlag(fs)
	path := fs.String("path", "", "folder to watch (required)")
	account := fs.String("account", "", "account to upload to (required)")
	fs.Parse(args)
	if *path == "" || *account == "" {
		return errors.New("--path and --account are required")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if err := cfg.AddFolder(config.Folder{Path: *path, Account: *account}); err != nil {
		return err
	}
	if err := cfg.Save(*cfgPath); err != nil {
		return err
	}
	fmt.Printf("Watching %s for account %q.\n", cfg.Folders[len(cfg.Folders)-1].Path, *account)
	notifyReload(ctx, *cfgPath)
	return nil
}

func cmdRemoveFolder(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("remove-folder", flag.ExitOnError)
	cfgPath := configFlag(fs)
	path := fs.String("path", "", "watched folder to remove (required)")
	fs.Parse(args)
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	ok, err := cfg.RemoveFolder(*path)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s is not a watched folder", *path)
	}
	if err := cfg.Save(*cfgPath); err != nil {
		return err
	}
	fmt.Println("Removed. Files already in Immich are not affected.")
	notifyReload(ctx, *cfgPath)
	return nil
}

func cmdList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	cfgPath := configFlag(fs)
	fs.Parse(args)
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	fmt.Println("Config:", *cfgPath)
	if len(cfg.Accounts) == 0 {
		fmt.Println("No accounts yet. Start with: immich-courier login --account NAME --server URL")
		return nil
	}
	// Totals come from the state DB, which only one process can open; if an
	// instance is running, use `status` for live numbers instead.
	var counts map[string]map[state.Status]int
	if c, err := daemon.Dial(*cfgPath); err == nil {
		if _, err := c.Status(ctx); err == nil {
			fmt.Println("(immich-courier is running; see `immich-courier status` for live numbers)")
		}
	} else if st, err := state.Open(daemon.StatePath(*cfgPath)); err == nil {
		counts, _ = st.Counts()
		st.Close()
	}
	for _, a := range cfg.Accounts {
		key := "API key in keychain"
		if _, err := keyring.Get(daemon.KeyringService, a.Name); err != nil {
			key = "NO API KEY (run login)"
		}
		fmt.Printf("\nAccount %q  %s  [%s]\n", a.Name, a.Server, key)
		if c := counts[a.Name]; c != nil {
			fmt.Printf("  uploaded %d, already in Immich %d, waiting %d, failed %d, rejected %d\n",
				c[state.Uploaded], c[state.Duplicate], c[state.Queued], c[state.Failed], c[state.Rejected])
		}
		for _, f := range cfg.Folders {
			if f.Account == a.Name {
				fmt.Println("  watching", f.Path)
			}
		}
	}
	return nil
}

func cmdLogin(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	cfgPath := configFlag(fs)
	account := fs.String("account", "", "account profile name, e.g. personal (required)")
	server := fs.String("server", "", "Immich server URL, e.g. https://photos.example.com (required)")
	method := fs.String("method", "auto", "auto, oidc, password or apikey")
	email := fs.String("email", "", "email for password login (prompted if empty)")
	port := fs.Int("port", auth.DefaultCallbackPort, "loopback port for OIDC login")
	fs.Parse(args)
	if *account == "" || *server == "" {
		fs.Usage()
		return errors.New("--account and --server are required")
	}

	c := immich.New(*server)
	if err := c.Ping(ctx); err != nil {
		return fmt.Errorf("can't reach Immich at %s: %w", *server, err)
	}
	ver, _ := c.Version(ctx)
	feat, err := c.Features(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Immich %s at %s (OAuth: %v, password login: %v)\n", ver, *server, feat.OAuth, feat.PasswordLogin)

	m := *method
	if m == "auto" {
		switch {
		case feat.OAuth:
			m = "oidc"
		case feat.PasswordLogin:
			m = "password"
		default:
			m = "apikey"
		}
	}

	var secret string
	switch m {
	case "oidc", "password":
		if m == "oidc" && !feat.OAuth {
			return errors.New("this server doesn't have OAuth enabled")
		}
		if m == "password" && !feat.PasswordLogin {
			return errors.New("this server doesn't allow password login")
		}
		var s auth.Session
		if m == "oidc" {
			fmt.Printf("Opening your browser to log in. If it doesn't open, use this URL:\n")
			s, err = auth.OIDCLogin(ctx, c, auth.OIDCOptions{
				Port:  *port,
				OnURL: func(u string) { fmt.Println("  " + u) },
			})
			if err != nil {
				return fmt.Errorf("%w\n\nIf the identity provider rejected the redirect URI, the Immich admin needs to add\n  http://127.0.0.1:%d/callback\nas an allowed redirect URI for Immich's OAuth client. Or use --method password or --method apikey", err, *port)
			}
		} else {
			if *email == "" {
				*email = prompt("Email: ")
			}
			pw, err := promptSecret("Password: ")
			if err != nil {
				return err
			}
			s, err = auth.PasswordLogin(ctx, c, *email, pw)
			if err != nil {
				return err
			}
		}
		fmt.Printf("Logged in as %s <%s>\n", s.Name, s.Email)
		host, _ := os.Hostname()
		key, err := auth.MintAPIKey(ctx, *server, s, "immich-courier ("+host+")")
		if err != nil && key.Secret == "" {
			return err
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "warning:", err)
		}
		fmt.Printf("Created API key %q with permissions %v\n", key.Name, key.Permissions)
		secret = key.Secret
	case "apikey":
		fmt.Printf("Create an API key in Immich (Account settings → API keys) with at least %v,\nthen paste it here.\n", auth.Permissions)
		_ = auth.OpenBrowser(strings.TrimRight(*server, "/") + "/user-settings?isOpen=api-keys")
		secret, err = promptSecret("API key: ")
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown method %q", m)
	}

	// Verify the key before storing it.
	kc := immich.New(*server)
	kc.APIKey = secret
	if u, err := kc.Me(ctx); err != nil {
		return fmt.Errorf("the API key doesn't work: %w", err)
	} else {
		fmt.Printf("API key works: uploads will go to %s <%s>\n", u.Name, u.Email)
	}
	if err := keyring.Set(daemon.KeyringService, *account, secret); err != nil {
		return fmt.Errorf("store key in OS keychain: %w", err)
	}
	fmt.Printf("Stored in the OS keychain as %s/%s\n", daemon.KeyringService, *account)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	cfg.SetAccount(config.Account{Name: *account, Server: strings.TrimRight(*server, "/")})
	if err := cfg.Save(*cfgPath); err != nil {
		return err
	}
	fmt.Printf("Account %q saved to %s.\n", *account, *cfgPath)
	notifyReload(ctx, *cfgPath)
	return nil
}

func cmdLogout(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("logout", flag.ExitOnError)
	account := fs.String("account", "", "account profile name (required)")
	fs.Parse(args)
	if *account == "" {
		return errors.New("--account is required")
	}
	if err := keyring.Delete(daemon.KeyringService, *account); err != nil {
		return err
	}
	fmt.Println("Removed the key from the OS keychain. The API key itself still exists in Immich; delete it under Account settings → API keys if you no longer need it.")
	return nil
}

func prompt(label string) string {
	fmt.Print(label)
	s, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(s)
}

func promptSecret(label string) (string, error) {
	fmt.Print(label)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	return strings.TrimSpace(string(b)), err
}
