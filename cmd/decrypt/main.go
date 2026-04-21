package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/codernirdesh/pgdrive-backup/internal/decryptor"
	"github.com/codernirdesh/pgdrive-backup/internal/uploader"
)

type options struct {
	key         string
	input       string
	output      string
	download    string
	credentials string
	folderID    string
	tokenFile   string
	prefix      string
	action      string
	list        bool
	latest      bool
	selectIndex int
}

type writeTarget struct {
	writer io.Writer
	path   string
	close  func() error
}

var errUserAborted = errors.New("user aborted")

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[decrypt] ")

	opts := parseFlags()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, opts); err != nil {
		if errors.Is(err, errUserAborted) {
			log.Println("Cancelled")
			os.Exit(1)
		}
		log.Fatalf("%v", err)
	}
}

func run(ctx context.Context, opts options) error {
	if opts.input != "" {
		return decryptLocalFile(opts)
	}

	client, err := newDriveClient(ctx, opts)
	if err != nil {
		return err
	}

	backups, err := client.ListBackups(ctx, opts.prefix)
	if err != nil {
		return err
	}
	if len(backups) == 0 {
		return fmt.Errorf("no backups found in Google Drive folder")
	}

	sortBackups(backups)

	if opts.list {
		printBackups(backups)
		return nil
	}

	interactive := !opts.latest && opts.selectIndex == 0
	selected, err := chooseBackup(backups, opts, interactive)
	if err != nil {
		return err
	}

	action, err := resolveAction(opts.action, interactive)
	if err != nil {
		return err
	}

	return runDriveAction(ctx, client, selected, opts, action, interactive)
}

func parseFlags() options {
	var opts options

	flag.StringVar(&opts.key, "key", envOrDefault("ENCRYPTION_KEY", ""), "64-char hex encryption key (defaults to ENCRYPTION_KEY)")
	flag.StringVar(&opts.input, "input", "", "path to encrypted local backup file")
	flag.StringVar(&opts.output, "output", "", "decrypted output file path, directory, or - for stdout")
	flag.StringVar(&opts.download, "download", "", "download encrypted backup to this file path or existing directory")
	flag.StringVar(&opts.credentials, "credentials", envOrDefault("GOOGLE_DRIVE_CREDENTIALS", "credentials.json"), "Google Drive OAuth credentials JSON path")
	flag.StringVar(&opts.folderID, "folder-id", envOrDefault("GOOGLE_DRIVE_FOLDER_ID", ""), "Google Drive folder ID containing backups")
	flag.StringVar(&opts.tokenFile, "token", envOrDefault("GOOGLE_DRIVE_TOKEN_FILE", "token.json"), "Google Drive OAuth token cache file")
	flag.StringVar(&opts.prefix, "prefix", envOrDefault("BACKUP_PREFIX", ""), "optional backup name prefix filter")
	flag.StringVar(&opts.action, "action", "", "Drive action: decrypt, download, or both")
	flag.BoolVar(&opts.list, "list", false, "list backups in Google Drive and exit")
	flag.BoolVar(&opts.latest, "latest", false, "use the newest backup in Google Drive")
	flag.IntVar(&opts.selectIndex, "select", 0, "pick a backup by its 1-based position from the sorted list")

	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage:\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  decrypt -input <backup.enc> [-key <hex-key>] [-output <file-or-dir>]\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  decrypt -list [-prefix <prefix>]\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  decrypt [-latest | -select N] [-action decrypt|download|both] [-output <path>] [-download <path>]\n\n")
		fmt.Fprintf(flag.CommandLine.Output(), "Examples:\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  decrypt -list\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  decrypt                         # interactive Drive picker\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  decrypt -latest -action decrypt -output ./restore.dump\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  decrypt -select 2 -action download -download ./downloads\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  decrypt -input ./backup.sql.gz.enc -output ./restore.dump\n\n")
		fmt.Fprintf(flag.CommandLine.Output(), "Note: decrypted output is a pg_dump custom-format archive suitable for pg_restore.\n")
		flag.PrintDefaults()
	}

	flag.Parse()
	return opts
}

func decryptLocalFile(opts options) error {
	if opts.key == "" {
		return fmt.Errorf("-key is required for local decryption (or set ENCRYPTION_KEY)")
	}

	f, err := os.Open(opts.input)
	if err != nil {
		return fmt.Errorf("open input: %w", err)
	}
	defer f.Close()

	target, err := openTarget(opts.output, "", false)
	if err != nil {
		return err
	}
	defer closeTarget(target)

	n, err := decryptToWriter(f, opts.key, target.writer)
	if err != nil {
		return err
	}

	if target.path == "stdout" {
		log.Printf("Decrypted and decompressed %d bytes to stdout", n)
		return nil
	}

	log.Printf("Decrypted and decompressed %d bytes to %s", n, target.path)
	log.Printf("Restore with: pg_restore -d <database> %s", target.path)
	return nil
}

func newDriveClient(ctx context.Context, opts options) (*uploader.Client, error) {
	if opts.folderID == "" {
		return nil, fmt.Errorf("Google Drive mode requires -folder-id or GOOGLE_DRIVE_FOLDER_ID")
	}

	client, err := uploader.New(ctx, opts.credentials, opts.folderID, opts.tokenFile)
	if err != nil {
		return nil, fmt.Errorf("create drive client: %w", err)
	}

	return client, nil
}

func sortBackups(backups []uploader.BackupFile) {
	sort.Slice(backups, func(i, j int) bool {
		if backups[i].CreatedTime.Equal(backups[j].CreatedTime) {
			return backups[i].Name < backups[j].Name
		}
		return backups[i].CreatedTime.After(backups[j].CreatedTime)
	})
}

func printBackups(backups []uploader.BackupFile) {
	if len(backups) == 0 {
		fmt.Println("No backups found.")
		return
	}

	fmt.Println("Available backups (newest first):")
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "#\tCreated (UTC)\tSize\tName")
	for i, backup := range backups {
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n",
			i+1,
			backup.CreatedTime.UTC().Format("2006-01-02 15:04:05"),
			humanSize(backup.Size),
			backup.Name,
		)
	}
	_ = tw.Flush()
	if len(backups) > 0 {
		fmt.Printf("\nTip: run without flags for an interactive picker, or use -latest for the freshest backup.\n")
	}
}

func chooseBackup(backups []uploader.BackupFile, opts options, interactive bool) (uploader.BackupFile, error) {
	if opts.latest {
		return backups[0], nil
	}

	if opts.selectIndex > 0 {
		if opts.selectIndex > len(backups) {
			return uploader.BackupFile{}, fmt.Errorf("-select %d is out of range (1-%d)", opts.selectIndex, len(backups))
		}
		return backups[opts.selectIndex-1], nil
	}

	if !interactive {
		return uploader.BackupFile{}, fmt.Errorf("no backup selected")
	}

	printBackups(backups)
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Printf("\nChoose backup [1-%d, Enter=1, q=quit]: ", len(backups))
		choice, err := reader.ReadString('\n')
		if err != nil {
			return uploader.BackupFile{}, fmt.Errorf("read selection: %w", err)
		}

		choice = strings.TrimSpace(choice)
		if choice == "" {
			return backups[0], nil
		}
		if strings.EqualFold(choice, "q") {
			return uploader.BackupFile{}, errUserAborted
		}

		idx, err := strconv.Atoi(choice)
		if err != nil || idx < 1 || idx > len(backups) {
			fmt.Println("Please enter a valid number from the list.")
			continue
		}

		return backups[idx-1], nil
	}
}

func resolveAction(flagValue string, interactive bool) (string, error) {
	action := strings.ToLower(strings.TrimSpace(flagValue))
	if action == "" && !interactive {
		action = "decrypt"
	}

	if action == "" {
		reader := bufio.NewReader(os.Stdin)
		for {
			fmt.Print("Action? [d]ecrypt, [w] download encrypted, [b] both (default: decrypt): ")
			choice, err := reader.ReadString('\n')
			if err != nil {
				return "", fmt.Errorf("read action: %w", err)
			}

			switch strings.ToLower(strings.TrimSpace(choice)) {
			case "", "d", "decrypt":
				return "decrypt", nil
			case "w", "download":
				return "download", nil
			case "b", "both":
				return "both", nil
			default:
				fmt.Println("Please choose decrypt, download, or both.")
			}
		}
	}

	switch action {
	case "decrypt", "download", "both":
		return action, nil
	default:
		return "", fmt.Errorf("invalid -action %q (expected decrypt, download, or both)", flagValue)
	}
}

func runDriveAction(ctx context.Context, client *uploader.Client, backup uploader.BackupFile, opts options, action string, interactive bool) error {
	if (action == "decrypt" || action == "both") && opts.key == "" {
		return fmt.Errorf("decryption requires -key or ENCRYPTION_KEY")
	}

	rawTargetPath := opts.download
	if (action == "download" || action == "both") && interactive {
		defaultRaw := backup.Name
		resolved, err := promptPath("Encrypted download path", defaultRaw)
		if err != nil {
			return err
		}
		rawTargetPath = resolved
	}

	decryptTargetPath := opts.output
	if (action == "decrypt" || action == "both") && interactive {
		defaultOutput := defaultDecryptedName(backup.Name)
		resolved, err := promptPath("Decrypted output path", defaultOutput)
		if err != nil {
			return err
		}
		decryptTargetPath = resolved
	}

	reader, err := client.Download(ctx, backup.ID)
	if err != nil {
		return err
	}
	defer reader.Close()

	stream := io.Reader(reader)
	var rawTarget *writeTarget
	if action == "download" || action == "both" {
		rawTarget, err = openTarget(rawTargetPath, backup.Name, true)
		if err != nil {
			return err
		}
		defer closeTarget(rawTarget)
		if action == "both" {
			stream = io.TeeReader(stream, rawTarget.writer)
		}
	}

	switch action {
	case "download":
		n, err := io.Copy(rawTarget.writer, stream)
		if err != nil {
			return fmt.Errorf("download encrypted backup: %w", err)
		}
		log.Printf("Downloaded %d bytes to %s", n, rawTarget.path)
		return nil

	case "decrypt", "both":
		decryptTarget, err := openTarget(decryptTargetPath, defaultDecryptedName(backup.Name), true)
		if err != nil {
			return err
		}
		defer closeTarget(decryptTarget)

		n, err := decryptToWriter(stream, opts.key, decryptTarget.writer)
		if err != nil {
			return err
		}

		log.Printf("Decrypted %s (%s) to %s", backup.Name, humanSize(backup.Size), decryptTarget.path)
		log.Printf("Wrote %d bytes of pg_dump custom-format data", n)
		log.Printf("Restore with: pg_restore -d <database> %s", decryptTarget.path)
		if rawTarget != nil {
			log.Printf("Encrypted copy also saved to %s", rawTarget.path)
		}
		return nil
	default:
		return fmt.Errorf("unsupported action %q", action)
	}
}

func promptPath(label, defaultValue string) (string, error) {
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Printf("%s [%s]: ", label, defaultValue)
		value, err := reader.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("read %s: %w", strings.ToLower(label), err)
		}

		value = strings.TrimSpace(value)
		if strings.EqualFold(value, "q") {
			return "", errUserAborted
		}
		if value == "" {
			return defaultValue, nil
		}
		return value, nil
	}
}

func decryptToWriter(src io.Reader, key string, dst io.Writer) (int64, error) {
	decryptedReader, err := decryptor.NewDecryptingReader(src, key)
	if err != nil {
		return 0, fmt.Errorf("create decryptor: %w", err)
	}
	defer decryptedReader.Close()

	gzReader, err := gzip.NewReader(decryptedReader)
	if err != nil {
		return 0, fmt.Errorf("create gzip reader: %w", err)
	}
	defer gzReader.Close()

	n, err := io.Copy(dst, gzReader)
	if err != nil {
		return 0, fmt.Errorf("write output: %w", err)
	}

	return n, nil
}

func openTarget(path, defaultName string, defaultToFile bool) (*writeTarget, error) {
	if path == "" && !defaultToFile {
		return &writeTarget{
			writer: os.Stdout,
			path:   "stdout",
			close:  func() error { return nil },
		}, nil
	}

	resolved := path
	if resolved == "" {
		resolved = defaultName
	}
	if resolved == "-" {
		return &writeTarget{
			writer: os.Stdout,
			path:   "stdout",
			close:  func() error { return nil },
		}, nil
	}

	if info, err := os.Stat(resolved); err == nil && info.IsDir() {
		resolved = filepath.Join(resolved, defaultName)
	}

	dir := filepath.Dir(resolved)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create output directory: %w", err)
		}
	}

	f, err := os.Create(resolved)
	if err != nil {
		return nil, fmt.Errorf("create output file: %w", err)
	}

	return &writeTarget{
		writer: f,
		path:   resolved,
		close:  f.Close,
	}, nil
}

func closeTarget(target *writeTarget) {
	if target == nil || target.close == nil {
		return
	}
	if err := target.close(); err != nil {
		log.Printf("close %s: %v", target.path, err)
	}
}

func defaultDecryptedName(name string) string {
	base := strings.TrimSuffix(name, ".enc")
	base = strings.TrimSuffix(base, ".gz")
	base = strings.TrimSuffix(base, ".sql")
	if base == "" || base == "." {
		return "restore.dump"
	}
	if strings.HasSuffix(base, ".dump") {
		return base
	}
	return base + ".dump"
}

func humanSize(size int64) string {
	if size <= 0 {
		return "—"
	}

	units := []string{"B", "KB", "MB", "GB", "TB"}
	value := float64(size)
	unit := 0
	for value >= 1024 && unit < len(units)-1 {
		value /= 1024
		unit++
	}

	if unit == 0 {
		return fmt.Sprintf("%d %s", size, units[unit])
	}
	return fmt.Sprintf("%.1f %s", value, units[unit])
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
