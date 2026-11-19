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
	"path/filepath"
	"strings"

	"github.com/zalando/go-keyring"
	"golang.org/x/term"

	"immich-courier/internal/auth"
	"immich-courier/internal/config"
	"immich-courier/internal/engine"
	"immich-courier/internal/immich"
	"immich-courier/internal/state"
)

const keyringService = "immich-courier"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error
	switch os.Args[1] {
	case "run":
		err = cmdRun(ctx, os.Args[2:])
	case "login":
		err = cmdLogin(ctx, os.Args[2:])
	case "logout":
		err = cmdLogout(os.Args[2:])
	case "add-folder":
		err = cmdAddFolder(os.Args[2:])
	case "remove-folder":
		err = cmdRemoveFolder(os.Args[2:])
	case "list":
		err = cmdList(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `immich-courier: watch folders and upload new photos/videos to Immich

Commands:
  login          --account NAME --server URL [--method auto|oidc|password|apikey]
  logout         --account NAME
  add-folder     --path DIR --account NAME
  remove-folder  --path DIR
  list           show accounts, folders and upload counts
  run            watch folders and upload (runs until stopped)

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

func statePath(cfgPath string) string {
	return filepath.Join(filepath.Dir(cfgPath), "state.db")
}

func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := configFlag(fs)
	verbose := fs.Bool("v", false, "debug logging")
	fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	keys := map[string]string{}
	for _, f := range cfg.Folders {
		if _, done := keys[f.Account]; done {
			continue
		}
		k, err := keyring.Get(keyringService, f.Account)
		if err != nil {
			return fmt.Errorf("no API key for account %q in the OS keychain (%v); run login first", f.Account, err)
		}
		if strings.IndexFunc(k, func(r rune) bool { return r <= ' ' || r > '~' }) >= 0 {
			return fmt.Errorf("the API key stored for account %q is malformed; run login again", f.Account)
		}
		keys[f.Account] = k
	}
	if err := os.MkdirAll(filepath.Dir(*cfgPath), 0o700); err != nil {
		return err
	}
	store, err := state.Open(statePath(*cfgPath))
	if err != nil {
		return err
	}
	defer store.Close()
	e, err := engine.New(cfg, keys, store, log, engine.Options{
		Settle: cfg.Settings.Settle(),
		Rescan: cfg.Settings.Rescan(),
	})
	if err != nil {
		return err
	}
	log.Info("immich-courier started", "config", *cfgPath)
	err = e.Run(ctx)
	log.Info("immich-courier stopped")
	return err
}

func cmdAddFolder(args []string) error {
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
	fmt.Printf("Watching %s for account %q (takes effect when immich-courier run is (re)started)\n", cfg.Folders[len(cfg.Folders)-1].Path, *account)
	return nil
}

func cmdRemoveFolder(args []string) error {
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
	return nil
}

func cmdList(args []string) error {
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
	var counts map[string]map[state.Status]int
	if st, err := state.Open(statePath(*cfgPath)); err == nil {
		counts, _ = st.Counts()
		st.Close()
	} else {
		fmt.Println("(upload counts unavailable while immich-courier run is active)")
	}
	for _, a := range cfg.Accounts {
		_, keyErr := keyring.Get(keyringService, a.Name)
		key := "API key in keychain"
		if keyErr != nil {
			key = "NO API KEY (run login)"
		}
		fmt.Printf("\nAccount %q  %s  [%s]\n", a.Name, a.Server, key)
		if c := counts[a.Name]; c != nil {
			fmt.Printf("  uploaded %d, already in Immich %d, queued %d, failed %d, rejected %d\n",
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
	if err := keyring.Set(keyringService, *account, secret); err != nil {
		return fmt.Errorf("store key in OS keychain: %w", err)
	}
	fmt.Printf("Stored in the OS keychain as %s/%s\n", keyringService, *account)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	cfg.SetAccount(config.Account{Name: *account, Server: strings.TrimRight(*server, "/")})
	if err := cfg.Save(*cfgPath); err != nil {
		return err
	}
	fmt.Printf("Account %q saved to %s. Next: immich-courier add-folder --path DIR --account %s\n", *account, *cfgPath, *account)
	return nil
}

func cmdLogout(args []string) error {
	fs := flag.NewFlagSet("logout", flag.ExitOnError)
	account := fs.String("account", "", "account profile name (required)")
	fs.Parse(args)
	if *account == "" {
		return errors.New("--account is required")
	}
	if err := keyring.Delete(keyringService, *account); err != nil {
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
