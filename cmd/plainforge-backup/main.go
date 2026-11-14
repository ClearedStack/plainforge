// Command plainforge-backup runs nightly age-encrypted off-box backups and
// monthly isolated restore drills for Plainforge's bare Git repository catalog.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/ClearedStack/plainforge/internal/age"
	"github.com/ClearedStack/plainforge/internal/backup"
	"github.com/ClearedStack/plainforge/internal/pferr"
)

const usageText = "usage: plainforge-backup <backup | restore-drill | keygen> [flags]"

func run(ctx context.Context, args []string, getenv func(string) string, stdout io.Writer, logger *log.Logger) error {
	if len(args) == 0 {
		return pferr.Parsef("%s", usageText)
	}
	sub := args[0]
	rest := args[1:]

	switch sub {
	case "backup":
		var opts backup.BackupOptions
		fs := flag.NewFlagSet("plainforge-backup backup", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.StringVar(&opts.ReposDir, "repos", "", "root directory holding bare <name>.git repositories")
		fs.StringVar(&opts.ScratchDir, "scratch", "", "writable scratch directory for staging")
		fs.StringVar(&opts.RecipientFile, "recipient-file", getenv("PLAINFORGE_BACKUP_RECIPIENT_FILE"), "file holding the public age1... recipient key")
		fs.StringVar(&opts.DestURL, "dest-url", getenv("PLAINFORGE_BACKUP_URL"), "HTTPS URL for off-node PUT upload")
		fs.StringVar(&opts.DestTokenFile, "dest-token-file", getenv("PLAINFORGE_BACKUP_TOKEN_FILE"), "optional file holding bearer token for -dest-url")
		fs.StringVar(&opts.OutputFile, "output", "", "optional local file path to save the encrypted archive")
		if err := fs.Parse(rest); err != nil || fs.NArg() != 0 {
			return pferr.Parsef("usage: plainforge-backup backup -repos DIR -scratch DIR -recipient-file FILE [-dest-url URL] [-dest-token-file FILE] [-output FILE]")
		}
		sum, err := backup.RunBackup(ctx, opts)
		if err != nil {
			return pferr.Public(err, "backup failed")
		}
		logger.Printf("backup completed: %d repositories, %d refs, %d objects, %d bytes",
			sum.Repos, sum.Refs, sum.Objects, sum.Bytes)
		return nil

	case "restore-drill":
		var opts backup.DrillOptions
		fs := flag.NewFlagSet("plainforge-backup restore-drill", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.StringVar(&opts.ReposDir, "repos", "", "root directory holding bare <name>.git repositories")
		fs.StringVar(&opts.ScratchDir, "scratch", "", "writable scratch directory for isolated unpack and fsck")
		fs.StringVar(&opts.RecipientFile, "recipient-file", getenv("PLAINFORGE_BACKUP_RECIPIENT_FILE"), "optional recipient key file to validate")
		fs.StringVar(&opts.ArchiveFile, "archive", "", "optional encrypted .age archive to unpack and verify")
		fs.StringVar(&opts.IdentityFile, "identity-file", getenv("PLAINFORGE_BACKUP_IDENTITY_FILE"), "age secret key file when -archive is given")
		if err := fs.Parse(rest); err != nil || fs.NArg() != 0 {
			return pferr.Parsef("usage: plainforge-backup restore-drill -scratch DIR (-repos DIR [-recipient-file FILE] | -archive FILE -identity-file FILE)")
		}
		sum, err := backup.RunRestoreDrill(opts)
		if err != nil {
			return pferr.Public(err, "restore drill failed")
		}
		logger.Printf("restore drill verified: %d repositories, %d refs, %d objects, %d bytes",
			sum.Repos, sum.Refs, sum.Objects, sum.Bytes)
		return nil

	case "keygen":
		var outPath string
		fs := flag.NewFlagSet("plainforge-backup keygen", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.StringVar(&outPath, "output", "", "optional file path to write the generated age identity")
		if err := fs.Parse(rest); err != nil || fs.NArg() != 0 {
			return pferr.Parsef("usage: plainforge-backup keygen [-output FILE]")
		}
		id, err := age.GenerateIdentity()
		if err != nil {
			return pferr.Public(err, "key generation failed")
		}
		content := fmt.Sprintf("# public key: %s\n%s\n", id.Recipient().String(), id.String())
		if outPath == "" {
			_, err := io.WriteString(stdout, content)
			return err
		}
		if err := os.WriteFile(outPath, []byte(content), 0o600); err != nil {
			return pferr.Parsef("writing key file failed")
		}
		logger.Printf("generated age keypair")
		return nil

	default:
		return pferr.Parsef("%s", usageText)
	}
}

func main() {
	logger := log.New(os.Stderr, "plainforge-backup: ", 0)
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(48 << 20)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, os.Args[1:], os.Getenv, os.Stdout, logger); err != nil {
		logger.Print(err)
		os.Exit(1)
	}
}
