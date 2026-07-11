package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"cuento/internal/auth"
	"cuento/internal/db"
	"cuento/internal/store"
)

// systemActor is the actor every CLI mutation runs as: the seeded system user
// (id 1). Binding it INSIDE the factored funcs (not just the dispatch) keeps the
// "system actor" guarantee true regardless of caller, so tests pass a plain
// context.Background().
var systemActor = store.Actor{ID: 1}

// userCmd dispatches `cuento user <add|passwd|disable> ...`. It opens the db,
// builds a store, parses the sub-subcommand's flags, reads any password from
// stdin, and calls the matching factored func (userAdd/userPasswd/userDisable)
// which does the versioned store write under the system actor.
//
// Password input approach: the new password is read as a single line from
// STDIN (a prompt when interactive, a piped line for scripts/tests). This avoids
// a -password flag that would leak the secret into shell history/process args.
// The factored funcs take the password as a plain string so tests call them
// directly without touching stdin.
func userCmd(args []string) error {
	if len(args) < 1 {
		userUsage()
		return errors.New("user: missing sub-subcommand")
	}
	sub, rest := args[0], args[1:]

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch sub {
	case "add":
		return userAddCmd(ctx, rest)
	case "passwd":
		return userPasswdCmd(ctx, rest)
	case "disable":
		return userDisableCmd(ctx, rest)
	default:
		userUsage()
		return fmt.Errorf("user: unknown sub-subcommand %q", sub)
	}
}

func userUsage() {
	fmt.Fprintf(os.Stderr, "usage: cuento user <command> [flags] <username>\n\ncommands:\n"+
		"  add <username> [--admin] [--display \"Name\"]   create a user (password read from stdin)\n"+
		"  passwd <username>                             set a user's password (read from stdin)\n"+
		"  disable <username>                            disable a user (cannot log in)\n")
}

// openStore opens the configured db and returns a store plus a closer. The db is
// migrated by `serve`/`migrate`; the CLI assumes the schema exists.
func openStore(dbPath string) (*store.Store, func(), error) {
	sqldb, err := db.Open(dbPath)
	if err != nil {
		return nil, nil, err
	}
	return store.New(sqldb), func() { _ = sqldb.Close() }, nil
}

// parseInterspersed parses fs but, unlike a bare fs.Parse, tolerates flags
// AFTER positional arguments (stdlib flag stops at the first non-flag token). It
// re-parses around each positional, so `user add carol --admin`, `user add
// --admin carol`, and `user passwd frank -db /path` all work. fs.Parse consumes
// a value-taking flag (e.g. --display "Name") as a unit, so a flag value is
// never mistaken for a positional. Returns the collected positionals in order.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positionals []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		positionals = append(positionals, rest[0])
		rest = rest[1:]
	}
	return positionals, nil
}

// userAddArgs is the parsed shape of `user add`. Factored out so the flag
// handling (including interspersed --admin/--display) is unit-testable without
// touching a db or stdin.
type userAddArgs struct {
	dbPath   string
	username string
	display  string
	admin    bool
}

func parseUserAdd(args []string) (userAddArgs, error) {
	fs := flag.NewFlagSet("user add", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath, "path to the SQLite database file")
	admin := fs.Bool("admin", false, "grant the user admin (implies all permissions)")
	display := fs.String("display", "", "display name (defaults to the username)")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return userAddArgs{}, err
	}
	if len(pos) == 0 || pos[0] == "" {
		return userAddArgs{}, errors.New("user add: username required")
	}
	return userAddArgs{dbPath: *dbPath, username: pos[0], display: *display, admin: *admin}, nil
}

func userAddCmd(ctx context.Context, args []string) error {
	pa, err := parseUserAdd(args)
	if err != nil {
		userUsage()
		return err
	}

	password, err := readPassword(os.Stdin, "New password: ")
	if err != nil {
		return err
	}

	st, closeFn, err := openStore(pa.dbPath)
	if err != nil {
		return err
	}
	defer closeFn()

	if err := userAdd(ctx, st, pa.username, pa.display, pa.admin, password); err != nil {
		return err
	}
	fmt.Printf("created user %q\n", pa.username)
	return nil
}

func userPasswdCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("user passwd", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath, "path to the SQLite database file")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 || pos[0] == "" {
		userUsage()
		return errors.New("user passwd: username required")
	}
	username := pos[0]

	password, err := readPassword(os.Stdin, "New password: ")
	if err != nil {
		return err
	}

	st, closeFn, err := openStore(*dbPath)
	if err != nil {
		return err
	}
	defer closeFn()

	if err := userPasswd(ctx, st, username, password); err != nil {
		return err
	}
	fmt.Printf("password updated for %q\n", username)
	return nil
}

func userDisableCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("user disable", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath, "path to the SQLite database file")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 || pos[0] == "" {
		userUsage()
		return errors.New("user disable: username required")
	}
	username := pos[0]

	st, closeFn, err := openStore(*dbPath)
	if err != nil {
		return err
	}
	defer closeFn()

	if err := userDisable(ctx, st, username); err != nil {
		return err
	}
	fmt.Printf("disabled user %q\n", username)
	return nil
}

// readPassword reads one line from r. On a terminal it first prints prompt to
// stderr. The trailing newline is stripped; the raw content is otherwise kept
// verbatim (a password may contain spaces). An empty password is rejected.
func readPassword(r io.Reader, prompt string) (string, error) {
	if f, ok := r.(*os.File); ok {
		if info, err := f.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			fmt.Fprint(os.Stderr, prompt)
		}
	}
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read password: %w", err)
	}
	password := strings.TrimRight(line, "\r\n")
	if password == "" {
		return "", errors.New("password must not be empty")
	}
	return password, nil
}

// userAdd creates a user with a hashed password under the system actor. --admin
// sets is_admin; txn_perm defaults to "none" (admins imply all, D10; per-perm
// management is the admin UI, p13.2). An empty display falls back to username.
func userAdd(ctx context.Context, st *store.Store, username, display string, admin bool, password string) error {
	if display == "" {
		display = username
	}
	hash, err := auth.Hash(password)
	if err != nil {
		return err
	}
	ctx = store.WithActor(ctx, systemActor)
	if _, err := st.CreateUser(ctx, store.CreateUserInput{
		Username:     username,
		DisplayName:  display,
		PasswordHash: &hash,
		IsAdmin:      admin,
		TxnPerm:      "none",
	}); err != nil {
		return fmt.Errorf("add user %q: %w", username, err)
	}
	return nil
}

// userPasswd hashes a new password and stores it via the versioned
// SetUserPassword (op='update', hash-excluded snapshot) under the system actor.
func userPasswd(ctx context.Context, st *store.Store, username, password string) error {
	ctx = store.WithActor(ctx, systemActor)
	id, err := st.UserIDByUsername(ctx, username)
	if err != nil {
		return err
	}
	hash, err := auth.Hash(password)
	if err != nil {
		return err
	}
	if err := st.SetUserPassword(ctx, id, hash); err != nil {
		return fmt.Errorf("set password for %q: %w", username, err)
	}
	return nil
}

// userDisable disables a user via the versioned DisableUser (op='update',
// disabled_at set) under the system actor. A disabled user cannot log in.
func userDisable(ctx context.Context, st *store.Store, username string) error {
	ctx = store.WithActor(ctx, systemActor)
	id, err := st.UserIDByUsername(ctx, username)
	if err != nil {
		return err
	}
	if err := st.DisableUser(ctx, id); err != nil {
		return fmt.Errorf("disable %q: %w", username, err)
	}
	return nil
}
