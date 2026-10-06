// Package backupcheck verifies backups by restoring them into a temp dir and
// checking their integrity, so "backup exists but restore was never tested"
// cannot happen silently.
package backupcheck

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/kernel"
	_ "modernc.org/sqlite"
)

const (
	// SampleLimit is the max number of records whose file references are checked.
	SampleLimit = 200

	dataDBName = "data.db"
	auxDBName  = "auxiliary.db"
)

// Report is the result of a backup verification.
type Report struct {
	Name            string        `json:"name"`
	SizeBytes       int64         `json:"sizeBytes"`
	IntegrityOK     bool          `json:"integrityOk"`
	QuickCheckOK    bool          `json:"quickCheckOk"`
	Collections     int           `json:"collections"`
	Records         int64         `json:"records"`
	LiveCollections int           `json:"liveCollections"`
	LiveRecords     int64         `json:"liveRecords"`
	MissingFiles    int           `json:"missingFiles"`
	SampledRecords  int           `json:"sampledRecords"`
	IsLatest        bool          `json:"isLatest"`
	Duration        time.Duration `json:"duration"`
	Error           string        `json:"error,omitempty"`
}

// CollectionsMatch reports whether the backup and the live app have the same number of collections.
func (r Report) CollectionsMatch() bool { return r.Collections == r.LiveCollections }

// OK reports whether the backup passed every hard check: integrity, quick check,
// and (when the backup is the latest one) the collections count.
func (r Report) OK() bool {
	if r.Error != "" || !r.IntegrityOK || !r.QuickCheckOK {
		return false
	}
	return !r.IsLatest || r.CollectionsMatch()
}

// Backup is a stored backup archive.
type Backup struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// List returns the stored backup archives sorted from the newest to the oldest.
func List(ctx context.Context, app kernel.App) ([]Backup, error) {
	fsys, err := app.NewBackupsFilesystem()
	if err != nil {
		return nil, err
	}
	defer fsys.Close()
	fsys.SetContext(ctx)

	objs, err := fsys.List("")
	if err != nil {
		return nil, err
	}

	result := make([]Backup, 0, len(objs))
	for _, o := range objs {
		if !strings.HasSuffix(o.Key, ".zip") {
			continue
		}
		result = append(result, Backup{Name: o.Key, Size: o.Size, Modified: o.ModTime})
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Modified.Equal(result[j].Modified) {
			return result[i].Name > result[j].Name
		}
		return result[i].Modified.After(result[j].Modified)
	})

	return result, nil
}

// Latest returns the name of the newest backup.
func Latest(ctx context.Context, app kernel.App) (string, error) {
	list, err := List(ctx, app)
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "", errors.New("no backups found")
	}
	return list[0].Name, nil
}

// Verify restores the backup `name` into a temp dir (the zip is downloaded in
// full when stored on S3) and checks it. The returned error is non-nil only
// when the verification itself could not be performed at all; check failures
// are reported in the Report (and mirrored in Report.Error).
func Verify(ctx context.Context, app kernel.App, name string) (Report, error) {
	start := time.Now()
	r := Report{Name: name}

	err := verify(ctx, app, name, &r)
	if err != nil {
		r.Error = err.Error()
	}
	r.Duration = time.Since(start)

	return r, err
}

func verify(ctx context.Context, app kernel.App, name string, r *Report) error {
	if name == "" || name != path.Base(name) {
		return fmt.Errorf("invalid backup name %q", name)
	}

	if latest, err := Latest(ctx, app); err == nil {
		r.IsLatest = latest == name
	}

	// temp dir inside pb_data/.pb_temp_to_delete-like location is not needed:
	// the OS temp dir keeps the live data dir untouched.
	tmp, err := os.MkdirTemp("", "toki_backupcheck_")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	zipPath := filepath.Join(tmp, "backup.zip")
	size, err := download(ctx, app, name, zipPath)
	if err != nil {
		return fmt.Errorf("fetch backup: %w", err)
	}
	r.SizeBytes = size

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	defer zr.Close()

	// zip entries (storage files are only indexed, not extracted)
	entries := make(map[string]struct{}, len(zr.File))
	for _, f := range zr.File {
		entries[strings.TrimSuffix(f.Name, "/")] = struct{}{}
	}

	restoreDir := filepath.Join(tmp, "restore")
	if err := os.MkdirAll(restoreDir, 0o700); err != nil {
		return err
	}
	for _, dbName := range []string{dataDBName, auxDBName} {
		if err := extract(zr, dbName, filepath.Join(restoreDir, dbName)); err != nil {
			return err
		}
	}

	// integrity of both databases
	var errs []error
	r.IntegrityOK, r.QuickCheckOK = true, true
	for _, dbName := range []string{dataDBName, auxDBName} {
		ok, quick, err := checkDB(ctx, filepath.Join(restoreDir, dbName))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", dbName, err))
		}
		r.IntegrityOK = r.IntegrityOK && ok
		r.QuickCheckOK = r.QuickCheckOK && quick
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	// content of the restored data.db
	db, err := openRO(filepath.Join(restoreDir, dataDBName))
	if err != nil {
		return err
	}
	defer db.Close()

	if err := inspect(ctx, db, entries, !app.Settings().S3.Enabled, r); err != nil {
		return err
	}

	// live app numbers
	cols, err := app.FindAllCollections()
	if err != nil {
		return fmt.Errorf("live collections: %w", err)
	}
	r.LiveCollections = len(cols)
	for _, c := range cols {
		if c.IsView() {
			continue
		}
		n, err := app.CountRecords(c)
		if err != nil {
			return fmt.Errorf("live records of %q: %w", c.Name, err)
		}
		r.LiveRecords += n
	}

	return nil
}

func download(ctx context.Context, app kernel.App, name, dst string) (int64, error) {
	fsys, err := app.NewBackupsFilesystem()
	if err != nil {
		return 0, err
	}
	defer fsys.Close()
	fsys.SetContext(ctx)

	reader, err := fsys.GetReader(name)
	if err != nil {
		return 0, err
	}
	defer reader.Close()

	out, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, reader)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return n, err
}

func extract(zr *zip.ReadCloser, entry, dst string) error {
	for _, f := range zr.File {
		if f.Name != entry {
			continue
		}
		src, err := f.Open()
		if err != nil {
			return fmt.Errorf("extract %s: %w", entry, err)
		}
		defer src.Close()

		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, src)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("extract %s: %w", entry, err)
		}
		return nil
	}
	return fmt.Errorf("%s is missing from the backup archive", entry)
}

func openRO(p string) (*dbx.DB, error) {
	return dbx.Open("sqlite", "file:"+filepath.ToSlash(p)+"?mode=ro&_pragma=busy_timeout(5000)")
}

// checkDB runs PRAGMA integrity_check and quick_check. A query error (e.g.
// "file is not a database", "malformed") counts as a failed check.
func checkDB(ctx context.Context, p string) (integrity, quick bool, err error) {
	db, err := openRO(p)
	if err != nil {
		return false, false, err
	}
	defer db.Close()

	run := func(pragma string) (bool, error) {
		var rows []string
		if err := db.NewQuery("PRAGMA " + pragma).WithContext(ctx).Column(&rows); err != nil {
			return false, fmt.Errorf("%s: %w", pragma, err)
		}
		return len(rows) == 1 && rows[0] == "ok", nil
	}

	var ierr, qerr error
	integrity, ierr = run("integrity_check")
	quick, qerr = run("quick_check")
	if !integrity && ierr == nil {
		ierr = errors.New("integrity_check did not return ok")
	}
	if !quick && qerr == nil {
		qerr = errors.New("quick_check did not return ok")
	}
	return integrity, quick, errors.Join(ierr, qerr)
}

type collectionRow struct {
	Id     string `db:"id"`
	Name   string `db:"name"`
	Type   string `db:"type"`
	Fields string `db:"fields"`
}

func inspect(ctx context.Context, db *dbx.DB, entries map[string]struct{}, checkFiles bool, r *Report) error {
	var cols []collectionRow
	if err := db.NewQuery("SELECT id, name, type, fields FROM _collections").WithContext(ctx).All(&cols); err != nil {
		return fmt.Errorf("read _collections: %w", err)
	}
	r.Collections = len(cols)

	sampled := 0
	for _, c := range cols {
		if c.Type == "view" {
			continue
		}
		table := "`" + strings.ReplaceAll(c.Name, "`", "``") + "`"

		var n int64
		if err := db.NewQuery("SELECT COUNT(*) FROM " + table).WithContext(ctx).Row(&n); err != nil {
			return fmt.Errorf("count records of %q: %w", c.Name, err)
		}
		r.Records += n

		if !checkFiles || sampled >= SampleLimit {
			continue
		}

		var fields []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(c.Fields), &fields); err != nil {
			return fmt.Errorf("parse fields of %q: %w", c.Name, err)
		}
		var fileFields []string
		for _, f := range fields {
			if f.Type == "file" {
				fileFields = append(fileFields, f.Name)
			}
		}
		if len(fileFields) == 0 {
			continue
		}

		sel := "`id`"
		for _, f := range fileFields {
			sel += ", `" + strings.ReplaceAll(f, "`", "``") + "`"
		}
		q := db.NewQuery(fmt.Sprintf("SELECT %s FROM %s LIMIT %d", sel, table, SampleLimit-sampled)).WithContext(ctx)
		rows, err := q.Rows()
		if err != nil {
			return fmt.Errorf("sample records of %q: %w", c.Name, err)
		}
		for rows.Next() {
			row := dbx.NullStringMap{}
			if err := rows.ScanMap(row); err != nil {
				rows.Close()
				return err
			}
			sampled++
			for _, f := range fileFields {
				for _, fn := range fileNames(row[f].String) {
					key := path.Join("storage", c.Id, row["id"].String, fn)
					if _, ok := entries[key]; !ok {
						r.MissingFiles++
					}
				}
			}
		}
		rows.Close()
	}
	r.SampledRecords = sampled

	return nil
}

// fileNames decodes a stored file field value (a JSON array or a single name).
func fileNames(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if strings.HasPrefix(raw, "[") {
		var list []string
		if json.Unmarshal([]byte(raw), &list) == nil {
			return list
		}
		return nil
	}
	return []string{raw}
}
