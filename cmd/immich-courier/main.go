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
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/term"

	"immich-courier/internal/accounts"
	"immich-courier/internal/auth"
	"immich-courier/internal/config"
	"immich-courier/internal/daemon"
	"immich-courier/internal/immich"
	"immich-courier/internal/state"
	"immich-courier/internal/webui"
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
	ctx := shutdownContext()

	cmds := map[string]func(context.Context, []string) error{
		"tray":          cmdTray,
		"settings":      cmdSettings,
		"run":           cmdRun,
		"status":        cmdStatus,
		"pause":         ipcCmd("pause", (*daemon.Client).Pause, "Uploads paused. Files are still watched and queued."),
		"resume":        ipcCmd("resume", (*daemon.Client).Resume, "Uploads resumed."),
		"rescan":        ipcCmd("rescan", (*daemon.Client).Rescan, "Rescanning folders."),
		"stop":          ipcCmd("stop", (*daemon.Client).Quit, "Stopped."),
		"login":         cmdLogin,
		"logout":        cmdLogout,
		"headers":       cmdHeaders,
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

// shutdownContext is cancelled by Ctrl+C or SIGTERM (what launchd, systemd
// and logout use to stop a process), so both shut down cleanly: the queue
// and socket are closed and the log says why it stopped. A second signal
// gets the default behavior, so a stuck shutdown can still be forced.
func shutdownContext() context.Context {
	ctx, cancel := context.WithCancelCause(context.Background())
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		s := <-sigs
		signal.Stop(sigs)
		cancel(fmt.Errorf("received signal %q", s))
	}()
	return ctx
}

// stopReason is why ctx ended, for the log ("" if it hasn't).
func stopReason(ctx context.Context) string {
	if ctx.Err() == nil {
		return ""
	}
	return context.Cause(ctx).Error()
}

func usage() {
	fmt.Fprint(os.Stderr, `immich-courier: watch folders and upload new photos/videos to Immich

Start it without arguments to run in the system tray; set it up from the
tray's Settings page. The commands below are for scripting and servers.

Setup:
  settings       open the settings page of the running instance [--print]
  login          --account NAME --server URL [--method auto|oidc|password|apikey]
                 [--header NAME ...]  (custom headers for a proxy; values are prompted)
  logout         --account NAME
  headers        --account NAME [--header NAME ... | --clear]  show or replace custom headers
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
	ui := webui.New(d)
	defer ui.Close()
	d.OpenSettings = ui.URL // `immich-courier settings --print` gives the URL
	log.Info("immich-courier started", "version", version, "config", *cfgPath)
	err = d.Run(ctx)
	log.Info("immich-courier stopped", "reason", stopReason(ctx))
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
		fmt.Println("Not uploading:", safe(s.Problem))
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
			fmt.Println("  uploading:", safe(a.Uploading))
		}
		if a.LastError != "" {
			fmt.Println("  last error:", safe(a.LastError))
		}
		for _, f := range s.Folders {
			if f.Account == a.Name {
				fmt.Println("  watching", safe(f.Path))
			}
		}
	}
	return nil
}

// safe escapes control characters (terminal escape sequences, newlines)
// in text that comes from file names or servers, so it can't rewrite
// the terminal or fake extra status lines.
func safe(s string) string {
	if strings.IndexFunc(s, func(r rune) bool { return !strconv.IsGraphic(r) }) < 0 {
		return s
	}
	q := strconv.QuoteToGraphic(s)
	return q[1 : len(q)-1]
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
		if !accounts.HasKey(*cfgPath, a) {
			key = "NO API KEY (run login)"
		}
		fmt.Printf("\nAccount %q  %s  [%s]\n", a.Name, a.Server, key)
		if hs, err := accounts.LoadHeaders(*cfgPath, a); err != nil {
			fmt.Println("  custom headers:", err)
		} else if len(hs) > 0 {
			fmt.Println("  custom headers:", strings.Join(immich.HeaderNames(hs), ", "))
		}
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
	headerNames := headerFlag(fs)
	fs.Parse(args)
	if *account == "" || *server == "" {
		fs.Usage()
		return errors.New("--account and --server are required")
	}
	if err := accounts.ValidName(*account); err != nil {
		return err
	}
	// Without --header an existing account keeps its headers; with it, the
	// given set replaces them.
	var in []immich.Header
	var err error
	if len(*headerNames) > 0 {
		if in, err = promptHeaders(*headerNames); err != nil {
			return err
		}
	}
	hs, err := accounts.ResolveHeaders(*cfgPath, *account, *server, in)
	if err != nil {
		return err
	}
	warnPlainHTTP(*server, hs)
	info, err := accounts.Probe(ctx, *server, hs)
	if err != nil {
		return err
	}
	*server = info.Server
	c := accounts.Client(*server, hs)
	feat := info
	fmt.Printf("Immich %s at %s (OAuth: %v, password login: %v)\n", info.Version, *server, feat.OAuth, feat.PasswordLogin)

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
		var warn error
		secret, warn, err = accounts.KeyFromSession(ctx, c, s)
		if err != nil {
			return err
		}
		if warn != nil {
			fmt.Fprintln(os.Stderr, "warning:", warn)
		}
		fmt.Printf("Created an upload-only API key (permissions: %s)\n", strings.Join(auth.Permissions, ", "))
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

	u, err := accounts.Save(ctx, *cfgPath, *account, *server, secret, hs)
	if err != nil {
		return err
	}
	fmt.Printf("Uploads for account %q will go to %s <%s>. Key stored in the OS keychain.\n", *account, u.Name, u.Email)
	notifyReload(ctx, *cfgPath)
	return nil
}

func cmdSettings(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("settings", flag.ExitOnError)
	cfgPath := configFlag(fs)
	noBrowser := fs.Bool("print", false, "only print the URL (e.g. on a server, open it through an SSH tunnel)")
	fs.Parse(args)
	c, err := daemon.Dial(*cfgPath)
	if err != nil {
		return fmt.Errorf("%w; start it first (run immich-courier without arguments, or immich-courier run)", err)
	}
	u, err := c.SettingsURL(ctx)
	if err != nil {
		return err
	}
	if !*noBrowser {
		if err := auth.OpenBrowser(u); err == nil {
			fmt.Println("Opened settings in your browser.")
			return nil
		}
	}
	fmt.Println("Settings page (this link works once, within 2 minutes; run the command again for a new one):")
	fmt.Println("  " + u)
	return nil
}

func cmdLogout(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("logout", flag.ExitOnError)
	cfgPath := configFlag(fs)
	account := fs.String("account", "", "account profile name (required)")
	fs.Parse(args)
	if *account == "" {
		return errors.New("--account is required")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	a, ok := cfg.Account(*account)
	if !ok {
		return fmt.Errorf("no account named %q in %s", *account, *cfgPath)
	}
	if err := accounts.DeleteKey(*cfgPath, a); err != nil {
		return err
	}
	fmt.Println("Removed the key (and any custom headers) from the OS keychain. The API key itself still exists in Immich; delete it under Account settings → API keys if you no longer need it.")
	return nil
}

func cmdHeaders(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("headers", flag.ExitOnError)
	cfgPath := configFlag(fs)
	account := fs.String("account", "", "account profile name (required)")
	clear := fs.Bool("clear", false, "remove all custom headers")
	headerNames := headerFlag(fs)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: immich-courier headers --account NAME [--header NAME ... | --clear]

Custom headers are sent with every request to the account's server, for a
reverse proxy in front of Immich that has its own login (for example basic
auth: --header Authorization, value "Basic <base64 of user:password>").
Values are asked for and stored in the OS keychain, never in the config.

Without --header or --clear, lists the header names. --header replaces the
whole set; press Enter at a value prompt to keep that header's current value.
`)
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if *account == "" {
		fs.Usage()
		return errors.New("--account is required")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	a, ok := cfg.Account(*account)
	if !ok {
		return fmt.Errorf("no account named %q in %s", *account, *cfgPath)
	}
	if !*clear && len(*headerNames) == 0 {
		hs, err := accounts.LoadHeaders(*cfgPath, a)
		if err != nil {
			return err
		}
		if len(hs) == 0 {
			fmt.Printf("Account %q has no custom headers.\n", a.Name)
			return nil
		}
		fmt.Printf("Account %q sends: %s\n", a.Name, strings.Join(immich.HeaderNames(hs), ", "))
		return nil
	}
	if *clear && len(*headerNames) > 0 {
		return errors.New("use either --header or --clear")
	}
	var in []immich.Header // nil (with --clear) removes them all
	if !*clear {
		if in, err = promptHeaders(*headerNames); err != nil {
			return err
		}
		warnPlainHTTP(a.Server, in)
	}
	hs, err := accounts.SetHeaders(ctx, *cfgPath, a.Name, in)
	if err != nil {
		return err
	}
	if len(hs) == 0 {
		fmt.Printf("Account %q no longer sends custom headers.\n", a.Name)
	} else {
		fmt.Printf("Checked with %s. Account %q now sends: %s\n", a.Server, a.Name, strings.Join(immich.HeaderNames(hs), ", "))
	}
	notifyReload(ctx, *cfgPath)
	return nil
}

// headerFlag adds a repeatable --header NAME flag. Only names: values are
// prompted for, so they never land in shell history or the process list.
func headerFlag(fs *flag.FlagSet) *[]string {
	var names []string
	fs.Func("header", "custom header NAME to send to the server (repeatable; the value is prompted for)", func(s string) error {
		if strings.ContainsAny(s, ":=") {
			return errors.New("give only the header name; the value is asked for")
		}
		names = append(names, s)
		return nil
	})
	return &names
}

func promptHeaders(names []string) ([]immich.Header, error) {
	hs := make([]immich.Header, 0, len(names))
	for _, n := range names {
		v, err := promptSecret(fmt.Sprintf("Value for %s: ", n))
		if err != nil {
			return nil, err
		}
		hs = append(hs, immich.Header{Name: n, Value: v})
	}
	return hs, nil
}

// warnPlainHTTP warns when custom headers (often proxy passwords) would go
// over unencrypted http.
func warnPlainHTTP(server string, hs []immich.Header) {
	if len(hs) > 0 && strings.HasPrefix(strings.ToLower(strings.TrimSpace(server)), "http://") {
		fmt.Fprintln(os.Stderr, "warning: the server address is http://, so the custom headers are sent unencrypted.")
	}
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
