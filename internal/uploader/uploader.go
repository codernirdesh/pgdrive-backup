package uploader

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

// Client wraps the Google Drive API.
type Client struct {
	srv      *drive.Service
	folderID string
}

// BackupFile describes a backup stored in Google Drive.
type BackupFile struct {
	ID          string
	Name        string
	CreatedTime time.Time
	Size        int64
}

// New creates a Google Drive client using OAuth 2.0 credentials.
// On first run it prints an authorization URL — visit it, grant access, and
// paste the code back. The resulting token (with refresh token) is saved to
// tokenFile so subsequent runs are fully automatic.
func New(ctx context.Context, credentialsFile, folderID, tokenFile string) (*Client, error) {
	data, err := os.ReadFile(credentialsFile)
	if err != nil {
		return nil, fmt.Errorf("read credentials file: %w", err)
	}

	oauthCfg, err := google.ConfigFromJSON(data, drive.DriveScope)
	if err != nil {
		return nil, fmt.Errorf("parse OAuth credentials: %w", err)
	}

	tok, err := loadToken(tokenFile)
	if err != nil {
		tok, err = authorizeInteractive(ctx, oauthCfg)
		if err != nil {
			return nil, fmt.Errorf("authorize: %w", err)
		}
		if err := saveToken(tokenFile, tok); err != nil {
			return nil, fmt.Errorf("save token: %w", err)
		}
		log.Printf("token saved to %s", tokenFile)
	}

	srv, err := drive.NewService(ctx, option.WithHTTPClient(oauthCfg.Client(ctx, tok)))
	if err != nil {
		return nil, fmt.Errorf("create drive service: %w", err)
	}
	return &Client{srv: srv, folderID: folderID}, nil
}

func authorizeInteractive(ctx context.Context, cfg *oauth2.Config) (*oauth2.Token, error) {
	authURL := cfg.AuthCodeURL("state-token", oauth2.AccessTypeOffline)
	fmt.Printf("\nOpen this URL in your browser and authorize the application:\n\n  %s\n\nPaste the authorization code here: ", authURL)

	var code string
	if _, err := fmt.Scan(&code); err != nil {
		return nil, fmt.Errorf("read auth code: %w", err)
	}

	tok, err := cfg.Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("exchange code for token: %w", err)
	}
	return tok, nil
}

func loadToken(path string) (*oauth2.Token, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var tok oauth2.Token
	if err := json.NewDecoder(f).Decode(&tok); err != nil {
		return nil, err
	}
	return &tok, nil
}

func saveToken(path string, tok *oauth2.Token) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(tok)
}

// ListBackups returns backup-like files from the configured Drive folder.
func (c *Client) ListBackups(ctx context.Context, prefix string) ([]BackupFile, error) {
	query := fmt.Sprintf("'%s' in parents and trashed = false", c.folderID)
	if prefix != "" {
		query += fmt.Sprintf(" and name contains '%s'", prefix)
	}

	var backups []BackupFile
	pageToken := ""
	for {
		req := c.srv.Files.List().
			Context(ctx).
			Q(query).
			Fields("nextPageToken, files(id, name, createdTime, size)").
			SupportsAllDrives(true).
			IncludeItemsFromAllDrives(true).
			PageSize(100)

		if pageToken != "" {
			req = req.PageToken(pageToken)
		}

		result, err := req.Do()
		if err != nil {
			return nil, fmt.Errorf("list backups: %w", err)
		}

		for _, f := range result.Files {
			created, err := time.Parse(time.RFC3339, f.CreatedTime)
			if err != nil {
				log.Printf("skip %s: bad timestamp %q", f.Name, f.CreatedTime)
				continue
			}

			backups = append(backups, BackupFile{
				ID:          f.Id,
				Name:        f.Name,
				CreatedTime: created,
				Size:        f.Size,
			})
		}

		pageToken = result.NextPageToken
		if pageToken == "" {
			break
		}
	}

	return backups, nil
}

// Download opens a streaming reader for a Drive file.
func (c *Client) Download(ctx context.Context, fileID string) (io.ReadCloser, error) {
	res, err := c.srv.Files.Get(fileID).
		Context(ctx).
		SupportsAllDrives(true).
		Download()
	if err != nil {
		return nil, fmt.Errorf("download file %s: %w", fileID, err)
	}

	return res.Body, nil
}

// Upload streams data directly to Google Drive without buffering to disk.
func (c *Client) Upload(ctx context.Context, filename string, reader io.Reader) (*drive.File, error) {
	log.Printf("uploading %s", filename)

	meta := &drive.File{
		Name:    filename,
		Parents: []string{c.folderID},
	}

	file, err := c.srv.Files.Create(meta).
		Context(ctx).
		Media(reader).
		SupportsAllDrives(true).
		Do()
	if err != nil {
		return nil, fmt.Errorf("upload to drive: %w", err)
	}

	log.Printf("uploaded %s (ID: %s)", file.Name, file.Id)
	return file, nil
}

// DeleteOlderThan removes files from the configured folder that are older than the given duration.
func (c *Client) DeleteOlderThan(ctx context.Context, prefix string, maxAge time.Duration) (int, error) {
	cutoff := time.Now().Add(-maxAge)

	query := fmt.Sprintf("'%s' in parents and name contains '%s' and trashed = false",
		c.folderID, prefix)

	var deleted int
	pageToken := ""
	for {
		req := c.srv.Files.List().
			Context(ctx).
			Q(query).
			Fields("nextPageToken, files(id, name, createdTime)").
			SupportsAllDrives(true).
			IncludeItemsFromAllDrives(true).
			PageSize(100)

		if pageToken != "" {
			req = req.PageToken(pageToken)
		}

		result, err := req.Do()
		if err != nil {
			return deleted, fmt.Errorf("list files for cleanup: %w", err)
		}

		for _, f := range result.Files {
			created, err := time.Parse(time.RFC3339, f.CreatedTime)
			if err != nil {
				log.Printf("skip %s: bad timestamp %q", f.Name, f.CreatedTime)
				continue
			}

			if created.Before(cutoff) {
				if err := c.srv.Files.Delete(f.Id).Context(ctx).SupportsAllDrives(true).Do(); err != nil {
					log.Printf("failed to delete %s: %v", f.Name, err)
					continue
				}
				log.Printf("deleted %s (created %s)", f.Name, f.CreatedTime)
				deleted++
			}
		}

		pageToken = result.NextPageToken
		if pageToken == "" {
			break
		}
	}

	return deleted, nil
}
