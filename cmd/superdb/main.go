package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"superdb/internal/engine"
	"superdb/internal/server"
	"superdb/internal/storage"
	"superdb/internal/updater"
	"time"
)

var version = "dev"

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "-version") {
		fmt.Printf("superdb %s\n", version)
		return
	}
	command := "menu"
	args := os.Args[1:]
	globalProduction := false
	if len(args) > 0 && (args[0] == "-production" || args[0] == "--production") {
		globalProduction = true
		args = args[1:]
		if len(args) == 0 || strings.HasPrefix(args[0], "-") {
			command = "server"
		}
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}
	if command == "menu" {
		command = menu()
		if command == "" {
			return
		}
	}
	if command == "server" {
		runServer(args, globalProduction)
		return
	}
	if command == "status" {
		runStatus(args)
		return
	}
	if command == "backup" {
		runBackup(args)
		return
	}
	if command == "restore" {
		runRestore(args)
		return
	}
	if command == "recover" {
		runRecover(args)
		return
	}
	if command == "compact" {
		runCompact(args)
		return
	}
	if command == "update" {
		runUpdate(args)
		return
	}
	fmt.Printf("unknown command %q\n", command)
	printUsage()
}

func printUsage() {
	fmt.Println("usage: superdb [server|status|backup|restore|recover|compact|update] [common flags]")
	fmt.Println("       superdb --production [server flags]")
}

func menu() string {
	fmt.Println("SuperDB")
	fmt.Println("1) Start server")
	fmt.Println("2) Status")
	fmt.Println("3) Backup")
	fmt.Println("4) Restore")
	fmt.Println("5) Compact")
	fmt.Println("q) Quit")
	fmt.Print("Choose an option: ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.TrimSpace(line) {
	case "1":
		return "server"
	case "2":
		return "status"
	case "3":
		return "backup"
	case "4":
		return "restore"
	case "5":
		return "compact"
	default:
		return ""
	}
}

func flags(name string, args []string) (*flag.FlagSet, *string, *string, *string) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	data := fs.String("data-dir", "./SUPERDB", "SUPERDB storage directory")
	fs.StringVar(data, "data", "./SUPERDB", "alias for --data-dir")
	addr := fs.String("addr", "127.0.0.1:7654", "server listen/connect address")
	mode := fs.String("mode", "memory", "durability mode: memory, wal, or snapshot")
	return fs, data, addr, mode
}

func runServer(args []string, globalProduction bool) {
	fs, data, addr, mode := flags("server", args)
	backupDir := fs.String("backup-dir", "", "directory for automatic snapshot backups (disabled when empty)")
	backupInterval := fs.Duration("backup-interval", 1*time.Hour, "automatic backup interval")
	production := fs.Bool("production", globalProduction, "use the production performance and durability profile")
	profile := fs.String("profile", "standard", "server profile: standard or production")
	clusterAddr := fs.String("cluster-addr", "", "internal cluster listen address (empty disables cluster mode)")
	fs.StringVar(clusterAddr, "listen", "", "alias for --cluster-addr")
	advertise := fs.String("advertise", "", "advertised cluster address (defaults to --cluster-addr)")
	join := fs.String("join", "", "comma-separated seed cluster address(es) to join")
	fs.Parse(args)
	if *profile != "standard" && *profile != "production" {
		log.Fatal("profile must be standard or production")
	}
	if *profile == "production" {
		*production = true
	}
	if *production && *mode == "memory" {
		*mode = "wal"
	}
	if *backupDir != "" && *backupInterval <= 0 {
		log.Fatal("backup-interval must be greater than zero")
	}
	db := engine.New()
	if *mode == "wal" || *mode == "snapshot" {
		if e := server.LoadSnapshot(*data, db); e != nil {
			log.Fatal(e)
		}
	}
	if *mode == "wal" {
		if e := server.ReplayWAL(*data, db); e != nil {
			log.Fatal(e)
		}
	}
	// Cluster mode wraps the same local engine. Local mode (no
	// --cluster-addr) initializes no cluster components and keeps the
	// existing fast path with zero network overhead.
	if *clusterAddr != "" {
		adv := *advertise
		if adv == "" {
			adv = *clusterAddr
		}
		var seeds []string
		if *join != "" {
			for _, s := range strings.Split(*join, ",") {
				if s = strings.TrimSpace(s); s != "" {
					seeds = append(seeds, s)
				}
			}
		}
		node, err := server.StartClusterNode(server.ClusterConfig{
			DataDir: *data, ListenAddr: *clusterAddr, AdvertiseAddr: adv, JoinAddrs: seeds, DB: db,
		})
		if err != nil {
			log.Fatal(err)
		}
		defer node.Shutdown()
		st := node.Status()
		log.Printf("SuperDB cluster node %s cluster %s listening internal %s", st.NodeID, st.ClusterID, *clusterAddr)
	}
	var walWriter *storage.WALWriter
	if *mode == "wal" {
		var e error
		walWriter, e = storage.OpenWALWriter(storage.WALPath(*data))
		if e != nil {
			log.Fatal(e)
		}
		defer walWriter.Close()
	}
	if *backupDir != "" {
		go runAutomaticBackups(*backupDir, *backupInterval, db)
	}
	log.Printf("SuperDB listening on %s mode=%s data=%s", *addr, *mode, *data)
	ln, e := net.Listen("tcp", *addr)
	if e != nil {
		log.Fatal(e)
	}
	defer ln.Close()
	for {
		c, e := ln.Accept()
		if e != nil {
			log.Print(e)
			continue
		}
		go server.HandleWithOptions(c, db, *mode, *data, server.HandleOptions{Production: *production, WALWriter: walWriter})
	}
}

func runAutomaticBackups(dir string, interval time.Duration, db *engine.Database) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		if _, err := server.CreateBackup(dir, db); err != nil {
			log.Printf("automatic backup failed: %v", err)
		}
	}
}

func runStatus(args []string) {
	fs, data, addr, mode := flags("status", args)
	fs.Parse(args)
	fmt.Printf("SuperDB status\n  data: %s\n  address: %s\n  mode: %s\n", *data, *addr, *mode)
	for _, name := range []string{storage.SnapshotPath(*data), storage.WALPath(*data)} {
		info, err := os.Stat(name)
		if os.IsNotExist(err) {
			fmt.Printf("  %-20s missing\n", filepath.Base(name))
			continue
		}
		if err != nil {
			log.Printf("  %s: %v\n", name, err)
			continue
		}
		fmt.Printf("  %-20s %d bytes\n", filepath.Base(name), info.Size())
	}
}

func runBackup(args []string) {
	fs, data, _, _ := flags("backup", args)
	fs.Parse(args)
	remaining := fs.Args()
	destination := filepath.Join(*data, "backup")
	if len(remaining) > 0 {
		destination = remaining[0]
	}
	if err := os.MkdirAll(destination, 0755); err != nil {
		log.Fatal(err)
	}
	for _, source := range []string{storage.SnapshotPath(*data), storage.WALPath(*data)} {
		if err := copyFile(source, filepath.Join(destination, filepath.Base(source))); err != nil && !os.IsNotExist(err) {
			log.Fatal(err)
		}
	}
	fmt.Printf("Backup created at %s\n", destination)
}

func runRestore(args []string) {
	fs, data, _, _ := flags("restore", args)
	fs.Parse(args)
	if len(fs.Args()) != 1 {
		fmt.Println("usage: superdb restore <backup-dir> [--data-dir DIR]")
		return
	}
	sourceDir := fs.Args()[0]
	if err := os.MkdirAll(*data, 0755); err != nil {
		log.Fatal(err)
	}
	for _, name := range []string{"snapshot.spdb", "wal.spdb"} {
		if err := copyFile(filepath.Join(sourceDir, name), filepath.Join(*data, name)); err != nil && !os.IsNotExist(err) {
			log.Fatal(err)
		}
	}
	fmt.Printf("Restored backup into %s\n", *data)
}

func runRecover(args []string) {
	fs, data, _, _ := flags("recover", args)
	backupDir := fs.String("backup-dir", "./BACKUPS", "timestamped backup directory")
	fs.Parse(args)
	if len(fs.Args()) > 0 {
		*backupDir = fs.Args()[0]
	}
	path, err := server.RecoverLatest(*backupDir, *data)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Recovered %s into %s\n", path, *data)
}

func runCompact(args []string) {
	fs, data, _, _ := flags("compact", args)
	fs.Parse(args)
	db := engine.New()
	if err := server.LoadSnapshot(*data, db); err != nil {
		log.Fatal(err)
	}
	if err := storage.WriteSnapshot(storage.SnapshotPath(*data), db); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Compacted snapshot at %s\n", storage.SnapshotPath(*data))
}

func runUpdate(args []string) {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	repository := fs.String("repository", updater.DefaultRepository, "GitHub repository in owner/name form")
	releaseVersion := fs.String("version", "", "target release version, for example 0.1.1 (latest when omitted)")
	installDir := fs.String("install-dir", "", "directory containing the installed SuperDB binaries")
	check := fs.Bool("check", false, "check for an available update without installing it")
	fs.Parse(args)
	ctx := context.Background()
	release, err := updater.Latest(ctx, *repository)
	if err != nil {
		log.Fatal(err)
	}
	latest := updater.Version(release.TagName)
	if *check {
		if version != "dev" && updater.Version(version) == latest {
			fmt.Printf("SuperDB %s is up to date\n", version)
			return
		}
		fmt.Printf("SuperDB update available: %s\n", latest)
		return
	}
	target := *releaseVersion
	if target == "" {
		target = latest
	}
	if version != "dev" && updater.Version(version) == updater.Version(target) {
		fmt.Printf("SuperDB %s is already installed\n", version)
		return
	}
	if err := updater.Update(ctx, *repository, target, *installDir); err != nil {
		if errors.Is(err, updater.ErrUpdateScheduled) {
			fmt.Printf("SuperDB %s update scheduled; restart SuperDB to use it\n", updater.Version(target))
			return
		}
		log.Fatal(err)
	}
	fmt.Printf("Updated SuperDB to %s\n", updater.Version(target))
}

func copyFile(source, destination string) error {
	b, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(destination, b, 0644)
}
