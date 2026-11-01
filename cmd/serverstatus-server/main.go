// Command serverstatus-server runs the ServerStatus dashboard / collector and
// provides node-management subcommands.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
	_ "time/tzdata" // timezone database for minimal systems

	"golang.org/x/term"

	"github.com/Lightmani/ServerStatus/internal/protocol"
	"github.com/Lightmani/ServerStatus/internal/server"
)

var version = "dev"

const defaultConfig = "/etc/serverstatus/server.yaml"

const usage = `ServerStatus server %s

Usage:
  serverstatus-server [-c config] <command> [options]

Commands:
  serve                       run the server (default)
  node add -name NAME [...]   register a node and print its credentials
  node list                   list nodes
  node edit ID [...]          change node properties
  node secret ID              rotate a node's secret
  node del ID                 delete a node and all its data
  passwd [-clear]             set (or remove) the dashboard password
  config-example              print an example configuration file
  version                     print version

Global options:
  -c path   config file (default %s; defaults are used if it does not exist)
`

func main() {
	log.SetFlags(log.LstdFlags)
	fs := flag.NewFlagSet("serverstatus-server", flag.ExitOnError)
	cfgPath := fs.String("c", defaultConfig, "config file")
	fs.Usage = func() { fmt.Fprintf(os.Stderr, usage, version, defaultConfig) }
	_ = fs.Parse(os.Args[1:])
	explicit := false
	fs.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "c" })

	args := fs.Args()
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "version", "-v", "--version":
		fmt.Println(version)
		return
	case "config-example":
		fmt.Print(server.ExampleConfig)
		return
	case "help", "-h", "--help":
		fs.Usage()
		return
	}

	cfg, err := server.LoadConfig(*cfgPath, explicit)
	if err != nil {
		fatal(err)
	}
	switch cmd {
	case "serve":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := server.Run(ctx, cfg, version); err != nil {
			fatal(err)
		}
	case "node":
		err = nodeCmd(cfg, args)
		fixOwnership(cfg.Database)
	case "passwd":
		err = passwdCmd(cfg, args)
		fixOwnership(cfg.Database)
	default:
		fs.Usage()
		os.Exit(2)
	}
	if err != nil {
		fatal(err)
	}
}

// fixOwnership: when the CLI runs as root against the database of a server
// that runs as an unprivileged user, make sure the files SQLite may have
// created stay owned by that user (the owner of the database directory).
func fixOwnership(db string) {
	if os.Geteuid() != 0 {
		return
	}
	fi, err := os.Stat(filepath.Dir(db))
	if err != nil {
		return
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid == 0 {
		return
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Lchown(db+suffix, int(st.Uid), int(st.Gid))
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func openStore(cfg *server.Config) *server.Store {
	st, err := server.OpenStore(cfg.Database)
	if err != nil {
		fatal(err)
	}
	return st
}

var idRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

func randomID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// parseSize parses "500G", "1.5T", "1024", "800GB" into bytes (binary units).
func parseSize(s string) (uint64, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.TrimSuffix(strings.TrimSuffix(s, "IB"), "B")
	if s == "" || s == "0" {
		return 0, nil
	}
	mult := 1.0
	switch s[len(s)-1] {
	case 'K':
		mult = 1 << 10
	case 'M':
		mult = 1 << 20
	case 'G':
		mult = 1 << 30
	case 'T':
		mult = 1 << 40
	case 'P':
		mult = 1 << 50
	}
	if mult > 1 {
		s = s[:len(s)-1]
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return uint64(v * mult), nil
}

func fmtSize(b uint64) string {
	if b == 0 {
		return "unlimited"
	}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	v := float64(b)
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	return strconv.FormatFloat(v, 'f', -1, 64) + " " + units[i]
}

type nodeFlags struct {
	fs                           *flag.FlagSet
	name, group, location, limit *string
	sort, resetDay               *int
	hidden                       *bool
}

func newNodeFlags(name string) *nodeFlags {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	return &nodeFlags{
		fs:       fs,
		name:     fs.String("name", "", "display name"),
		group:    fs.String("group", "", "group name"),
		location: fs.String("location", "", "location label, e.g. \"Hong Kong\""),
		limit:    fs.String("limit", "0", "traffic quota per cycle, e.g. 1T, 500G (0 = unlimited)"),
		sort:     fs.Int("sort", 0, "sort order (smaller first)"),
		resetDay: fs.Int("reset-day", 1, "day of month the traffic cycle resets (1-31)"),
		hidden:   fs.Bool("hidden", false, "hide from the public dashboard"),
	}
}

// apply copies the explicitly-set flags onto n.
func (f *nodeFlags) apply(n *server.Node) error {
	var err error
	f.fs.Visit(func(fl *flag.Flag) {
		switch fl.Name {
		case "name":
			n.Name = *f.name
		case "group":
			n.Group = *f.group
		case "location":
			n.Location = *f.location
		case "sort":
			n.Sort = *f.sort
		case "reset-day":
			n.ResetDay = *f.resetDay
		case "hidden":
			n.Hidden = *f.hidden
		case "limit":
			n.TrafficLimit, err = parseSize(*f.limit)
		}
	})
	if err != nil {
		return err
	}
	if n.ResetDay < 1 || n.ResetDay > 31 {
		return errors.New("-reset-day must be between 1 and 31")
	}
	return nil
}

// parseInterspersed lets flags follow a positional ID (`node edit ID -name x`).
func parseInterspersed(fs *flag.FlagSet, args []string) (string, error) {
	var pos string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		pos, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if pos == "" && fs.NArg() > 0 {
		pos = fs.Arg(0)
	}
	return pos, nil
}

func nodeCmd(cfg *server.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: node add|list|edit|secret|del")
	}
	sub, args := args[0], args[1:]
	st := openStore(cfg)
	defer st.Close()

	switch sub {
	case "add":
		f := newNodeFlags("node add")
		id := f.fs.String("id", "", "node id (default: random)")
		_ = f.fs.Parse(args)
		n := server.Node{ID: *id, ResetDay: 1, Secret: protocol.NewSecret(), CreatedAt: time.Now().Unix()}
		if n.ID == "" {
			n.ID = randomID()
		}
		if !idRe.MatchString(n.ID) {
			return errors.New("id may only contain letters, digits, '.', '_' and '-' (max 64)")
		}
		if err := f.apply(&n); err != nil {
			return err
		}
		if n.Name == "" {
			n.Name = n.ID
		}
		if err := st.AddNode(n); err != nil {
			return err
		}
		printCredentials(cfg, n, "Node created")
	case "list", "ls":
		nodes, err := st.ListNodes()
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tGROUP\tLOCATION\tRESET\tLIMIT\tHIDDEN\tLAST SEEN")
		for _, n := range nodes {
			seen := "never"
			if n.LastSeen > 0 {
				seen = time.Unix(n.LastSeen, 0).In(cfg.Location()).Format("2006-01-02 15:04:05")
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%v\t%s\n", n.ID, n.Name, n.Group, n.Location, n.ResetDay, fmtSize(n.TrafficLimit), n.Hidden, seen)
		}
		tw.Flush()
	case "edit", "set":
		f := newNodeFlags("node edit")
		id, err := parseInterspersed(f.fs, args)
		if err != nil || id == "" {
			return errors.New("usage: node edit ID [-name N] [-group G] [-location L] [-sort N] [-reset-day D] [-limit SIZE] [-hidden=true|false]")
		}
		n, err := st.GetNode(id)
		if err != nil {
			return err
		}
		if err := f.apply(&n); err != nil {
			return err
		}
		if err := st.UpdateNodeMeta(n); err != nil {
			return err
		}
		fmt.Println("Node updated (the running server picks this up within 10s).")
	case "secret":
		if len(args) != 1 {
			return errors.New("usage: node secret ID")
		}
		n, err := st.GetNode(args[0])
		if err != nil {
			return err
		}
		n.Secret = protocol.NewSecret()
		if err := st.UpdateNodeMeta(n); err != nil {
			return err
		}
		printCredentials(cfg, n, "Secret rotated; update the agent config")
	case "del", "rm", "delete":
		fs := flag.NewFlagSet("node del", flag.ExitOnError)
		yes := fs.Bool("y", false, "do not ask for confirmation")
		id, _ := parseInterspersed(fs, args)
		if id == "" {
			return errors.New("usage: node del [-y] ID")
		}
		n, err := st.GetNode(id)
		if err != nil {
			return err
		}
		if !*yes && !confirm(fmt.Sprintf("Delete node %s (%s) and ALL of its history?", n.ID, n.Name)) {
			return errors.New("aborted")
		}
		if err := st.DeleteNode(id); err != nil {
			return err
		}
		fmt.Println("Node deleted.")
	default:
		return fmt.Errorf("unknown node command %q", sub)
	}
	return nil
}

func confirm(q string) bool {
	fmt.Printf("%s [y/N] ", q)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

func printCredentials(cfg *server.Config, n server.Node, title string) {
	url := cfg.PublicURL
	if url == "" {
		url = "http://YOUR_SERVER" + cfg.Listen
		if strings.HasPrefix(cfg.Listen, ":") {
			url = "http://YOUR_SERVER_IP" + cfg.Listen
		}
	}
	fmt.Printf(`%s.

  ID:     %s
  Name:   %s
  Secret: %s

Agent config (/etc/serverstatus/agent.yaml on the VPS):

server: "%s"
node_id: "%s"
secret: "%s"

Or install the agent on the VPS in one go (from the extracted release):

  sudo ./install.sh agent --server '%s' --id '%s' --secret '%s'

`, title, n.ID, n.Name, n.Secret, url, n.ID, n.Secret, url, n.ID, n.Secret)
}

func passwdCmd(cfg *server.Config, args []string) error {
	fs := flag.NewFlagSet("passwd", flag.ExitOnError)
	clear := fs.Bool("clear", false, "remove the password (dashboard becomes public)")
	stdin := fs.Bool("stdin", false, "read the password from stdin (for scripts)")
	_ = fs.Parse(args)
	st := openStore(cfg)
	defer st.Close()

	if *clear {
		if err := st.SetKV("dashboard_password", ""); err != nil {
			return err
		}
		fmt.Println("Password removed; the dashboard is now public.")
		return nil
	}
	var pw string
	if *stdin || !term.IsTerminal(int(os.Stdin.Fd())) {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return err
		}
		pw = strings.TrimRight(line, "\r\n")
	} else {
		fmt.Print("New dashboard password: ")
		b1, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return err
		}
		fmt.Print("Repeat: ")
		b2, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return err
		}
		if string(b1) != string(b2) {
			return errors.New("passwords do not match")
		}
		pw = string(b1)
	}
	if len(pw) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	hash, err := server.HashPassword(pw)
	if err != nil {
		return err
	}
	if err := st.SetKV("dashboard_password", hash); err != nil {
		return err
	}
	// rotate the session key so existing sessions are invalidated
	if err := st.SetKV("session_key", server.NewSessionKey()); err != nil {
		return err
	}
	fmt.Println("Password set. The running server applies it within 10s.")
	return nil
}
