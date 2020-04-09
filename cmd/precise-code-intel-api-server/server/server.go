package server

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/keegancsmith/sqlf"
	"github.com/klauspost/compress/gzip"
	"github.com/sourcegraph/sourcegraph/cmd/frontend/db"
	"github.com/sourcegraph/sourcegraph/internal/api"
	"github.com/sourcegraph/sourcegraph/internal/db/dbutil"
	"github.com/sourcegraph/sourcegraph/internal/gitserver"
	"github.com/sourcegraph/sourcegraph/internal/trace/ot"
	"github.com/tomnomnom/linkheader"
)

const DefaultUploadPageSize = 50

type Server struct {
	host             string
	port             int
	bundleManagerURL string
	db               *sql.DB
}

type ServerOpts struct {
	Host             string
	Port             int
	BundleManagerURL string
	DB               *sql.DB
}

func New(opts ServerOpts) *Server {
	return &Server{
		host:             opts.Host,
		port:             opts.Port,
		bundleManagerURL: opts.BundleManagerURL,
		db:               opts.DB,
	}
}

func (s *Server) Start() error {
	addr := net.JoinHostPort(s.host, strconv.FormatInt(int64(s.port), 10))
	handler := ot.Middleware(s.handler())
	server := &http.Server{Addr: addr, Handler: handler}

	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}

	return nil
}

// NOTE: the stuff below is pretty rough and I'm not planning on putting too much
// effort into this while we're doing the port. This is an internal API so it's
// allowed to be a bit shoddy during this transitionary period. I'm not even sure
// if HTTP is the right transport for the long term.

func (s *Server) handler() http.Handler {
	mux := mux.NewRouter()
	mux.Path("/uploads/{id:[0-9]+}").Methods("GET").HandlerFunc(s.handleGetUploadByID)
	mux.Path("/uploads/{id:[0-9]+}").Methods("DELETE").HandlerFunc(s.handleDeleteUploadByID)
	mux.Path("/uploads/repository/{id:[0-9]+}").Methods("GET").HandlerFunc(s.handleGetUploadsByRepo)
	mux.Path("/upload").Methods("POST").HandlerFunc(s.handleEnqueue)
	mux.Path("/exists").Methods("GET").HandlerFunc(s.handleExists)
	mux.Path("/definitions").Methods("GET").HandlerFunc(s.handleDefinitions)
	mux.Path("/references").Methods("GET").HandlerFunc(s.handleReferences)
	mux.Path("/hover").Methods("GET").HandlerFunc(s.handleHover)
	mux.Path("/uploads").Methods("POST").HandlerFunc(s.handleUploads)
	mux.Path("/prune").Methods("POST").HandlerFunc(s.handlePrune)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// GET /uploads/{id:[0-9]+}
func (s *Server) handleGetUploadByID(w http.ResponseWriter, r *http.Request) {
	id := idFromRequest(r)

	query := `
		SELECT
			u.id,
			u.commit,
			u.root,
			u.visible_at_tip,
			u.uploaded_at,
			u.state,
			u.failure_summary,
			u.failure_stacktrace,
			u.started_at,
			u.finished_at,
			u.tracing_context,
			u.repository_id,
			u.indexer,
			s.rank
		FROM lsif_uploads u
		LEFT JOIN (
			SELECT r.id, RANK() OVER (ORDER BY r.uploaded_at) as rank
			FROM lsif_uploads r
			WHERE r.state = 'queued'
		) s
		ON u.id = s.id
		WHERE u.id = $1
	`

	row := s.db.QueryRowContext(context.Background(), query, id)

	upload := struct {
		ID                int        `json:"id"`
		Commit            string     `json:"commit"`
		Root              string     `json:"root"`
		VisibleAtTip      bool       `json:"visibleAtTip"`
		UploadedAt        time.Time  `json:"uploadedAt"`
		State             string     `json:"state"`
		FailureSummary    *string    `json:"failureSummary"`
		FailureStacktrace *string    `json:"failureStacktrace"`
		StartedAt         *time.Time `json:"startedAt"`
		FinishedAt        *time.Time `json:"finishedAt"`
		TracingContext    string     `json:"tracingContext"`
		RepositoryID      int        `json:"repositoryId"`
		Indexer           string     `json:"indexer"`
		Rank              *int       `json:"placeInQueue"`
		// TODO - add this as an optional field
		// ProcessedAt       time.Time  `json:"processedAt"`
	}{}

	if err := row.Scan(
		&upload.ID,
		&upload.Commit,
		&upload.Root,
		&upload.VisibleAtTip,
		&upload.UploadedAt,
		&upload.State,
		&upload.FailureSummary,
		&upload.FailureStacktrace,
		&upload.StartedAt,
		&upload.FinishedAt,
		&upload.TracingContext,
		&upload.RepositoryID,
		&upload.Indexer,
		&upload.Rank,
	); err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "", http.StatusNotFound)
			return
		}

		fmt.Printf("WHOOPS: %s\n", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	_ = json.NewEncoder(w).Encode(upload)
}

// DELETE /uploads/{id:[0-9]+}
func (s *Server) handleDeleteUploadByID(w http.ResponseWriter, r *http.Request) {
	err := dbutil.Transaction(context.Background(), s.db, func(tx *sql.Tx) error {
		id := idFromRequest(r)
		query := "DELETE FROM lsif_uploads WHERE id = $1 RETURNING repository_id, visible_at_tip"

		var repositoryID int
		var visibleAtTip bool
		if err := tx.QueryRowContext(context.Background(), query, id).Scan(&repositoryID, &visibleAtTip); err != nil {
			return err
		}

		if visibleAtTip {
			// TODO - do we need this dependency?
			repo, err := db.Repos.Get(context.Background(), api.RepoID(repositoryID))
			if err != nil {
				return err
			}

			cmd := gitserver.DefaultClient.Command("git", "rev-parse", "HEAD")
			cmd.Repo = gitserver.Repo{Name: repo.Name}
			out, err := cmd.CombinedOutput(context.Background())
			if err != nil {
				return err
			}
			tipCommit := string(bytes.TrimSpace(out))

			// TODO - do we need to discover commits here? The old
			// implementation does it but my gut says no now that
			// I think about it a bit more.

			query2 := "WITH " + ancestorLineage + ", " + visibleDumps + `
				-- Update dump records by:
				--   (1) unsetting the visibility flag of all previously visible dumps, and
				--   (2) setting the visibility flag of all currently visible dumps
				UPDATE lsif_dumps d
				SET visible_at_tip = id IN (SELECT * from visible_ids)
				WHERE d.repository_id = $1 AND (d.id IN (SELECT * from visible_ids) OR d.visible_at_tip)
			`

			if _, err := tx.ExecContext(context.Background(), query2, repositoryID, tipCommit); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "", http.StatusNotFound)
			return
		}

		fmt.Printf("WHOOPS: %s\n", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// GET /uploads/repository/{id:[0-9]+}
func (s *Server) handleGetUploadsByRepo(w http.ResponseWriter, r *http.Request) {
	id := idFromRequest(r)
	q := r.URL.Query()
	term := q.Get("query")
	state := q.Get("state")
	visibleAtTip, _ := strconv.ParseBool(q.Get("visibleAtTip"))
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil {
		limit = DefaultUploadPageSize
	}
	offset, _ := strconv.Atoi(q.Get("offset"))

	conds := []*sqlf.Query{
		sqlf.Sprintf("u.repository_id = %s", id),
	}
	if state != "" {
		conds = append(conds, sqlf.Sprintf("state = %s", state))
	}
	if term != "" {
		var termConds []*sqlf.Query
		for _, column := range []string{"commit", "root", "indexer", "failure_summary", "failure_stacktrace"} {
			termConds = append(termConds, sqlf.Sprintf(column+" LIKE %s", "%"+term+"%"))
		}

		conds = append(conds, sqlf.Sprintf("(%s)", sqlf.Join(termConds, " OR ")))
	}
	if visibleAtTip {
		conds = append(conds, sqlf.Sprintf("visible_at_tip = true"))
	}

	query := sqlf.Sprintf(`
		SELECT
			u.id,
			u.commit,
			u.root,
			u.visible_at_tip,
			u.uploaded_at,
			u.state,
			u.failure_summary,
			u.failure_stacktrace,
			u.started_at,
			u.finished_at,
			u.tracing_context,
			u.repository_id,
			u.indexer,
			s.rank
		FROM lsif_uploads u
		LEFT JOIN (
			SELECT r.id, RANK() OVER (ORDER BY r.uploaded_at) as rank
			FROM lsif_uploads r
			WHERE r.state = 'queued'
		) s
		ON u.id = s.id
		WHERE %s
		ORDER BY uploaded_at DESC
		LIMIT %d
		OFFSET %d
	`, sqlf.Join(conds, " AND "), limit, offset)

	rows, err := s.db.QueryContext(context.Background(), query.Query(sqlf.PostgresBindVar), query.Args()...)
	if err != nil {
		fmt.Printf("WHOOPS: %s\n", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var uploads []interface{}
	for rows.Next() {
		upload := struct {
			ID                int        `json:"id"`
			Commit            string     `json:"commit"`
			Root              string     `json:"root"`
			VisibleAtTip      bool       `json:"visibleAtTip"`
			UploadedAt        time.Time  `json:"uploadedAt"`
			State             string     `json:"state"`
			FailureSummary    *string    `json:"failureSummary"`
			FailureStacktrace *string    `json:"failureStacktrace"`
			StartedAt         *time.Time `json:"startedAt"`
			FinishedAt        *time.Time `json:"finishedAt"`
			TracingContext    string     `json:"tracingContext"`
			RepositoryID      int        `json:"repositoryId"`
			Indexer           string     `json:"indexer"`
			Rank              *int       `json:"placeInQueue"`
		}{}

		if err := rows.Scan(
			&upload.ID,
			&upload.Commit,
			&upload.Root,
			&upload.VisibleAtTip,
			&upload.UploadedAt,
			&upload.State,
			&upload.FailureSummary,
			&upload.FailureStacktrace,
			&upload.StartedAt,
			&upload.FinishedAt,
			&upload.TracingContext,
			&upload.RepositoryID,
			&upload.Indexer,
			&upload.Rank,
		); err != nil {
			fmt.Printf("WHOOPS: %s\n", err)
			http.Error(w, "", http.StatusInternalServerError)
			return
		}

		uploads = append(uploads, upload)
	}

	count := len(uploads) // TODO - perform this

	if offset+len(uploads) < count {
		q := r.URL.Query()
		q.Set("limit", strconv.FormatInt(int64(limit), 10))
		q.Set("offset", strconv.FormatInt(int64(offset+len(uploads)), 10))
		r.URL.RawQuery = q.Encode()
		link := linkheader.Link{
			URL: r.URL.String(),
			Rel: "next",
		}
		w.Header().Set("Link", link.String())
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{"uploads": uploads, "totalCount": count})
}

// POST /upload
func (s *Server) handleEnqueue(w http.ResponseWriter, r *http.Request) {
	sanitize := func(s string) string {
		if s == "" || s == "/" {
			return ""
		}
		if strings.HasSuffix(s, "/") {
			return s
		}
		return s + "/"
	}

	q := r.URL.Query()
	repositoryID := q.Get("repositoryId")
	commit := q.Get("commit")
	root := sanitize(q.Get("root"))
	indexerName := q.Get("indexerName")

	f, err := ioutil.TempFile("", "upload-")
	if err != nil {
		fmt.Printf("WHOOPS: %s\n", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	defer os.Remove(f.Name())
	defer f.Close()

	if _, err := io.Copy(f, r.Body); err != nil {
		fmt.Printf("WHOOPS: %s\n", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	if indexerName == "" {
		if _, err := f.Seek(0, 0); err != nil {
			fmt.Printf("WHOOPS: %s\n", err)
			http.Error(w, "", http.StatusInternalServerError)
			return
		}

		gzipReader, err := gzip.NewReader(f)
		if err != nil {
			fmt.Printf("WHOOPS: %s\n", err)
			http.Error(w, "", http.StatusInternalServerError)
			return
		}

		line, isPrefix, err := bufio.NewReader(gzipReader).ReadLine()
		if err != nil {
			fmt.Printf("WHOOPS: %s\n", err)
			http.Error(w, "", http.StatusInternalServerError)
			return
		}
		if isPrefix {
			// OOF strange condition in these parts
			fmt.Printf("WHOOPS: %s\n", err)
			http.Error(w, "", http.StatusInternalServerError)
			return
		}

		meta := struct {
			Label    string `json:"label"`
			ToolInfo struct {
				Name string `json:"name"`
			} `json:"toolInfo"`
		}{}

		if err := json.Unmarshal(line, &meta); err != nil {
			fmt.Printf("WHOOPS R: %s\n", err)
			http.Error(w, "", http.StatusInternalServerError)
			return
		}

		if meta.Label != "metaData" || meta.ToolInfo.Name == "" {
			panic("no indexer name") // TODO
		}
		indexerName = meta.ToolInfo.Name
	}

	tracingContext := "{}" // TODO

	var id int
	if err := dbutil.Transaction(context.Background(), s.db, func(tx *sql.Tx) error {
		if err := s.db.QueryRowContext(
			context.Background(),
			`INSERT INTO lsif_uploads (commit, root, tracing_context, repository_id, indexer) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			commit, root, tracingContext, repositoryID, indexerName,
		).Scan(&id); err != nil {
			return err
		}

		if _, err := f.Seek(0, 0); err != nil {
			return err
		}

		url, err := url.Parse(fmt.Sprintf("%s/uploads/%d", s.bundleManagerURL, id))
		if err != nil {
			return err
		}

		req, err := http.NewRequest("POST", url.String(), f)
		if err != nil {
			return err
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}

		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("bad status %d", resp.StatusCode)
		}

		return nil
	}); err != nil {
		fmt.Printf("WHOOPS: %s\n", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusAccepted)
	w.Write([]byte(fmt.Sprintf(`{"id": %d}`, id)))
}

type Dump struct {
	ID                int        `json:"id"`
	Commit            string     `json:"commit"`
	Root              string     `json:"root"`
	VisibleAtTip      bool       `json:"visibleAtTip"`
	UploadedAt        time.Time  `json:"uploadedAt"`
	State             string     `json:"state"`
	FailureSummary    *string    `json:"failureSummary"`
	FailureStacktrace *string    `json:"failureStacktrace"`
	StartedAt         *time.Time `json:"startedAt"`
	FinishedAt        *time.Time `json:"finishedAt"`
	TracingContext    string     `json:"tracingContext"`
	RepositoryID      int        `json:"repositoryId"`
	Indexer           string     `json:"indexer"`
	// TODO
	// ProcessedAt       time.Time  `json:"processedAt"`
}

func (s *Server) findClosestDatabase(repositoryID int, commit, file string) ([]Dump, error) {
	query := "WITH " + bidirectionalLineage + ", " + visibleDumps + `
		SELECT d.dump_id FROM lineage_with_dumps d
		WHERE $3 LIKE (d.root || '%') AND d.dump_id IN (SELECT * FROM visible_ids)
		ORDER BY d.n
	`
	rows, err := s.db.QueryContext(context.Background(), query, repositoryID, commit, file)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var qs []*sqlf.Query
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}

		qs = append(qs, sqlf.Sprintf("%d", id))
	}

	query2 := sqlf.Sprintf(`SELECT
		u.id,
		u.commit,
		u.root,
		u.visible_at_tip,
		u.uploaded_at,
		u.state,
		u.failure_summary,
		u.failure_stacktrace,
		u.started_at,
		u.finished_at,
		u.tracing_context,
		u.repository_id,
		u.indexer
	FROM lsif_uploads u WHERE id IN (%s)`, sqlf.Join(qs, ", "))

	rows2, err := s.db.QueryContext(context.Background(), query2.Query(sqlf.PostgresBindVar), query2.Args()...)
	if err != nil {
		return nil, err
	}
	defer rows2.Close()

	var dumps []Dump
	for rows2.Next() {
		dump := Dump{}
		if err := rows2.Scan(
			&dump.ID,
			&dump.Commit,
			&dump.Root,
			&dump.VisibleAtTip,
			&dump.UploadedAt,
			&dump.State,
			&dump.FailureSummary,
			&dump.FailureStacktrace,
			&dump.StartedAt,
			&dump.FinishedAt,
			&dump.TracingContext,
			&dump.RepositoryID,
			&dump.Indexer,
		); err != nil {
			return nil, err
		}

		db := Database{
			bundleManagerURL: s.bundleManagerURL,
			dumpID:           dump.ID,
		}

		// TODO - do these requests in parallel
		exists, err := db.Exists(pathToRoot(dump.Root, file))
		if err != nil {
			return nil, err
		}

		if exists {
			// TODO - need to de-duplicate
			dumps = append(dumps, dump)
		}
	}

	return dumps, nil
}

func pathToRoot(root, file string) string {
	if strings.HasPrefix(file, root) {
		return strings.TrimPrefix(file, root)
	}
	return file
}

// GET /exists
func (s *Server) handleExists(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	repositoryID, _ := strconv.Atoi(q.Get("repositoryId"))
	commit := q.Get("commit")
	file := q.Get("path")

	fmt.Printf("EXISTS\n")
	dumps, err := s.findClosestDatabase(repositoryID, commit, file)
	fmt.Printf("Dumps: %#v\n", dumps)
	fmt.Printf("Err: %#v\n", err)
	if err != nil {
		fmt.Printf("WHOOPSRRRRR: %s\n", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	rrr, err := json.Marshal(map[string]interface{}{"uploads": dumps})
	fmt.Printf("OK HERES UR DUMPS: %s\n\n\n\n", rrr)

	_ = json.NewEncoder(w).Encode(map[string]interface{}{"uploads": dumps})
}

func (s *Server) lookupMoniker(dumpID int, path string, moniker MonikerData, model string, skip, take *int) ([]Location, int, error) {
	if moniker.PackageInformationID == "" {
		return nil, 0, nil
	}

	db := Database{
		dumpID:           dumpID,
		bundleManagerURL: s.bundleManagerURL,
	}
	pid, err := db.PackageInformation(path, moniker.PackageInformationID)
	if err != nil {
		return nil, 0, err
	}

	dump, exists, err := s.getPackage(moniker.Scheme, pid.Name, pid.Version)
	if err != nil {
		return nil, 0, err
	}
	if !exists {
		return nil, 0, nil
	}

	db2 := Database{
		dumpID:           dump.ID,
		bundleManagerURL: s.bundleManagerURL,
	}
	return db2.MonikerResults(model, moniker.Scheme, moniker.Identifier, skip, take)
}

func (s *Server) getPackage(scheme, name, version string) (Dump, bool, error) {
	query := `
		SELECT
			u.id,
			u.commit,
			u.root,
			u.visible_at_tip,
			u.uploaded_at,
			u.state,
			u.failure_summary,
			u.failure_stacktrace,
			u.started_at,
			u.finished_at,
			u.tracing_context,
			u.repository_id,
			u.indexer
		FROM lsif_packages p
		JOIN lsif_uploads u ON p.dump_id = u.id
		WHERE p.scheme = $1 AND p.name = $2 AND p.version = $3
		LIMIT 1
	`

	dump := Dump{}
	if err := s.db.QueryRowContext(context.Background(), query, scheme, name, version).Scan(
		&dump.ID,
		&dump.Commit,
		&dump.Root,
		&dump.VisibleAtTip,
		&dump.UploadedAt,
		&dump.State,
		&dump.FailureSummary,
		&dump.FailureStacktrace,
		&dump.StartedAt,
		&dump.FinishedAt,
		&dump.TracingContext,
		&dump.RepositoryID,
		&dump.Indexer,
	); err != nil {
		if err == sql.ErrNoRows {
			return Dump{}, false, nil
		}

		return Dump{}, false, err
	}

	return dump, false, nil
}

func (s *Server) getDefs(dump Dump, db Database, pathInDb string, line, character int) ([]LocationThingers, error) {
	locations, err := db.Definitions(pathInDb, line, character)
	if err != nil {
		return nil, err
	}

	if len(locations) > 0 {
		return s.resolveLocations(dump.Root, locations), nil
	}

	rangeMonikers, err := db.MonikersByPosition(pathInDb, line, character)
	if err != nil {
		return nil, err
	}

	for _, monikers := range rangeMonikers {
		for _, moniker := range monikers {
			if moniker.Kind == "import" {
				results, _, err := s.lookupMoniker(dump.ID, pathInDb, moniker, "definition", nil, nil)
				if err != nil {
					return nil, err
				}

				x := s.resolveLocations(dump.Root, results)
				if len(x) > 0 {
					return x, nil
				}
			} else {
				// This symbol was not imported from another database. We search the definitions
				// table of our own database in case there was a definition that wasn't properly
				// attached to a result set but did have the correct monikers attached.

				results, _, err := db.MonikerResults("definition", moniker.Scheme, moniker.Identifier, nil, nil)
				if err != nil {
					return nil, err
				}

				x := s.resolveLocations(dump.Root, results)
				if len(x) > 0 {
					return x, nil
				}
			}
		}
	}

	return nil, nil
}

// GET /definitions
func (s *Server) handleDefinitions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// repositoryID, _ := strconv.Atoi(q.Get("repositoryId"))
	// commit := q.Get("commit")
	file := q.Get("path")
	line, _ := strconv.Atoi(q.Get("line"))
	character, _ := strconv.Atoi(q.Get("character"))
	uploadID, _ := strconv.Atoi(q.Get("uploadId"))

	dump, db, exists, err := s.getDumpAndDatabase(uploadID)
	if err != nil {
		fmt.Printf("WHOOPS: %s\n", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if !exists {
		http.Error(w, "", http.StatusNotFound)
		return
	}

	pathInDb := pathToDb(dump.Root, file)

	defs, err := s.getDefs(dump, db, pathInDb, line, character)
	if err != nil {
		fmt.Printf("WHOOPS: %s\n", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	resolved, err := s.resolveLocations2(defs)
	if err != nil {
		fmt.Printf("WHOOPS: %s\n", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	var outers []struct {
		RepositoryID int    `json:"repositoryId"`
		Commit       string `json:"commit"`
		Path         string `json:"path"`
		Range        Range  `json:"range"`
	}
	for _, res := range resolved {
		outers = append(outers, struct {
			RepositoryID int    `json:"repositoryId"`
			Commit       string `json:"commit"`
			Path         string `json:"path"`
			Range        Range  `json:"range"`
		}{
			RepositoryID: res.Dump.RepositoryID,
			Commit:       res.Dump.Commit,
			Path:         res.Path,
			Range:        res.Range,
		})
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{"locations": outers})
}

type LocationThingers struct {
	DumpID int    `json:"dumpId"`
	Path   string `json:"path"`
	Range  Range  `json:"Range"`
}

type LocationThingers2 struct {
	Dump  Dump   `json:"dump"`
	Path  string `json:"path"`
	Range Range  `json:"range"`
}

func (s *Server) resolveLocations(root string, locations []Location) []LocationThingers {
	var thingers []LocationThingers
	for _, location := range locations {
		thingers = append(thingers, LocationThingers{
			DumpID: location.DumpID,
			Path:   root + location.Path,
			Range:  location.Range,
		})
	}

	return thingers
}

func (s *Server) resolveLocations2(locations []LocationThingers) ([]LocationThingers2, error) {
	var thingers []LocationThingers2

	uniq := map[int]struct{}{}
	for _, l := range locations {
		uniq[l.DumpID] = struct{}{}
	}

	var qs []*sqlf.Query
	for id := range uniq {
		qs = append(qs, sqlf.Sprintf("%d", id))
	}

	query2 := sqlf.Sprintf(`SELECT
		u.id,
		u.commit,
		u.root,
		u.visible_at_tip,
		u.uploaded_at,
		u.state,
		u.failure_summary,
		u.failure_stacktrace,
		u.started_at,
		u.finished_at,
		u.tracing_context,
		u.repository_id,
		u.indexer
	FROM lsif_uploads u WHERE id IN (%s)`, sqlf.Join(qs, ", "))

	rows2, err := s.db.QueryContext(context.Background(), query2.Query(sqlf.PostgresBindVar), query2.Args()...)
	if err != nil {
		return nil, err
	}
	defer rows2.Close()

	dumpsByID := map[int]Dump{}
	for rows2.Next() {
		dump := Dump{}
		if err := rows2.Scan(
			&dump.ID,
			&dump.Commit,
			&dump.Root,
			&dump.VisibleAtTip,
			&dump.UploadedAt,
			&dump.State,
			&dump.FailureSummary,
			&dump.FailureStacktrace,
			&dump.StartedAt,
			&dump.FinishedAt,
			&dump.TracingContext,
			&dump.RepositoryID,
			&dump.Indexer,
		); err != nil {
			return nil, err
		}

		dumpsByID[dump.ID] = dump
	}

	for _, location := range locations {
		thingers = append(thingers, LocationThingers2{
			Dump:  dumpsByID[location.DumpID],
			Path:  location.Path,
			Range: location.Range,
		})
	}

	return thingers, nil
}

// TODO - cursor t'ain't no string
func (s *Server) handlePagination(w http.ResponseWriter, repositoryID int, commit string, remoteDumpLimit, limit int, cursor Cursor) {
	// TODO - get resolved

	var outers []struct {
		RepositoryID int    `json:"repositoryId"`
		Commit       string `json:"commit"`
		Path         string `json:"path"`
		Range        Range  `json:"range"`
	}
	for _, res := range resolved {
		outers = append(outers, struct {
			RepositoryID int    `json:"repositoryId"`
			Commit       string `json:"commit"`
			Path         string `json:"path"`
			Range        Range  `json:"range"`
		}{
			RepositoryID: res.Dump.RepositoryID,
			Commit:       res.Dump.Commit,
			Path:         res.Path,
			Range:        res.Range,
		})
	}

	// TODO - encode cursor
	// TODO - link header
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"locations": outers})
}

type Cursor struct {
	Phase       string
	DumpID      int
	Path        string
	Line        int
	Character   int
	Monikers    []MonikerData
	SkipResults int

	// TODO - other thingers
}

func decodeCursor(raw string) (Cursor, error) {
	return Cursor{}, fmt.Errorf("Unimplemented") // TODO
}

func encodeCursor(cursor Cursor) string {
	return "" // TODO
}

// GET /references
func (s *Server) handleReferences(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	repositoryID, _ := strconv.Atoi(q.Get("repositoryId"))
	commit := q.Get("commit")
	file := q.Get("path")
	line, _ := strconv.Atoi(q.Get("line"))
	character, _ := strconv.Atoi(q.Get("character"))
	uploadID, _ := strconv.Atoi(q.Get("uploadId"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	cursor := q.Get("cursor")

	if cursor != "" {
		realCursor, err := decodeCursor(cursor)
		if err != nil {
			fmt.Printf("WHOOPS: %s\n", err)
			http.Error(w, "", http.StatusInternalServerError)
			return
		}

		s.handlePagination(w, repositoryID, commit, 20, limit, realCursor)
		return
	} else {
		dump, db, exists, err := s.getDumpAndDatabase(uploadID)
		if err != nil {
			fmt.Printf("WHOOPS: %s\n", err)
			http.Error(w, "", http.StatusInternalServerError)
			return
		}
		if !exists {
			http.Error(w, "", http.StatusNotFound)
			return
		}

		pathInDb := pathToDb(dump.Root, file)

		rangeMonikers, err := db.MonikersByPosition(pathInDb, line, character)
		if err != nil {
			fmt.Printf("WHOOPS: %s\n", err)
			http.Error(w, "", http.StatusInternalServerError)
			return
		}

		var flattened []MonikerData
		for _, monikers := range rangeMonikers {
			flattened = append(flattened, monikers...)
		}

		newCursor := Cursor{
			Phase:       "same-dump",
			DumpID:      dump.ID,
			Path:        pathInDb,
			Line:        line,
			Character:   character,
			Monikers:    flattened,
			SkipResults: 0,
		}

		s.handlePagination(w, repositoryID, commit, 20, limit, newCursor)
		return
	}
}

func (s *Server) getDumpAndDatabase(uploadID int) (Dump, Database, bool, error) {
	query := sqlf.Sprintf(`
	SELECT
		u.id,
		u.commit,
		u.root,
		u.visible_at_tip,
		u.uploaded_at,
		u.state,
		u.failure_summary,
		u.failure_stacktrace,
		u.started_at,
		u.finished_at,
		u.tracing_context,
		u.repository_id,
		u.indexer
	FROM lsif_uploads u WHERE id = %d
`, uploadID)

	var dump Dump
	if err := s.db.QueryRowContext(context.Background(), query.Query(sqlf.PostgresBindVar), query.Args()...).Scan(
		&dump.ID,
		&dump.Commit,
		&dump.Root,
		&dump.VisibleAtTip,
		&dump.UploadedAt,
		&dump.State,
		&dump.FailureSummary,
		&dump.FailureStacktrace,
		&dump.StartedAt,
		&dump.FinishedAt,
		&dump.TracingContext,
		&dump.RepositoryID,
		&dump.Indexer,
	); err != nil {
		if err == sql.ErrNoRows {
			return Dump{}, Database{}, false, nil
		}
		return Dump{}, Database{}, false, err
	}

	db := Database{
		bundleManagerURL: s.bundleManagerURL,
		dumpID:           dump.ID,
	}

	return dump, db, true, nil
}

// GET /hover
func (s *Server) handleHover(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// repositoryID, _ := strconv.Atoi(q.Get("repositoryId"))
	// commit := q.Get("commit")
	file := q.Get("path")
	line, _ := strconv.Atoi(q.Get("line"))
	character, _ := strconv.Atoi(q.Get("character"))
	uploadID, _ := strconv.Atoi(q.Get("uploadId"))

	dump, db, exists, err := s.getDumpAndDatabase(uploadID)
	if err != nil {
		fmt.Printf("WHOOPS: %s\n", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if !exists {
		http.Error(w, "", http.StatusNotFound)
		return
	}

	pathx := pathToDb(dump.Root, file)
	text, rn, exists, err := db.Hover(pathx, line, character)
	if err != nil {
		fmt.Printf("WHOOPS: %s\n", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if exists {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"text": text, "range": rn})
		return
	}

	defs, err := s.getDefs(dump, db, pathx, line, character)
	if err != nil {
		fmt.Printf("WHOOPS: %s\n", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	resolved, err := s.resolveLocations2(defs)
	if err != nil {
		fmt.Printf("WHOOPS: %s\n", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	if len(resolved) == 0 {
		_ = json.NewEncoder(w).Encode(nil)
		return
	}

	db2 := Database{
		dumpID:           resolved[0].Dump.ID,
		bundleManagerURL: s.bundleManagerURL,
	}

	p := pathToDb(resolved[0].Dump.Root, resolved[0].Path)
	text, rn, exists, err = db2.Hover(p, resolved[0].Range.Start.Line, resolved[0].Range.Start.Character)
	if err != nil {
		fmt.Printf("WHOOPS: %s\n", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if exists {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"text": text, "range": rn})
		return
	}

	_ = json.NewEncoder(w).Encode(nil)
	return
}

func pathToDb(root, path string) string {
	if strings.HasPrefix(path, root) {
		return strings.TrimPrefix(path, root)
	}
	return path
}

// POST /uploads
func (s *Server) handleUploads(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close() // TODO - search for this, can remove everywhere safely

	payload := struct {
		IDs []int `json:"ids"`
	}{}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		fmt.Printf("WHOOPS XXX: %s\n", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	var qs []*sqlf.Query
	for _, id := range payload.IDs {
		qs = append(qs, sqlf.Sprintf("%d", id))
	}

	query := sqlf.Sprintf("SELECT id, state FROM lsif_uploads WHERE id IN (%s)", sqlf.Join(qs, ", "))

	rows, err := s.db.QueryContext(context.Background(), query.Query(sqlf.PostgresBindVar), query.Args()...)
	if err != nil {
		fmt.Printf("WHOOPS: %s\n", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	states := map[int]string{}
	for rows.Next() {
		var id int
		var state string
		if err := rows.Scan(&id, &state); err != nil {
			fmt.Printf("WHOOPS: %s\n", err)
			http.Error(w, "", http.StatusInternalServerError)
			return
		}

		states[id] = state
	}

	pairs := []interface{}{}
	for k, v := range states {
		pairs = append(pairs, []interface{}{k, v})
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{"type": "map", "value": pairs})
}

// POST /prune
func (s *Server) handlePrune(w http.ResponseWriter, r *http.Request) {
	query := "DELETE FROM lsif_uploads WHERE visible_at_tip = false ORDER BY uploaded_at LIMIT 1 RETURNING id"

	var id int
	if err := s.db.QueryRowContext(context.Background(), query).Scan(&id); err != nil {
		if err == sql.ErrNoRows {
			_ = json.NewEncoder(w).Encode(nil)
			return
		}

		fmt.Printf("WHOOPS: %s\n", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": id})
}

// idFromRequest returns the database id from the request URL's path. This method
// must only be called from routes containing the `id:[0-9]+` pattern, as the error
// return from ParseInt is not checked.
func idFromRequest(r *http.Request) int64 {
	id, _ := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	return id
}

var ancestorLineage = `
	RECURSIVE lineage(id, "commit", parent, repository_id) AS (
		SELECT c.* FROM lsif_commits c WHERE c.repository_id = $1 AND c."commit" = $2
		UNION
		SELECT c.* FROM lineage a JOIN lsif_commits c ON a.repository_id = c.repository_id AND a.parent = c."commit"
	)
`

var bidirectionalLineage = `
	RECURSIVE lineage(id, "commit", parent_commit, repository_id, direction) AS (
		SELECT l.* FROM (
			-- seed recursive set with commit looking in ancestor direction
			SELECT c.*, 'A' FROM lsif_commits c WHERE c.repository_id = $1 AND c."commit" = $2
			UNION
			-- seed recursive set with commit looking in descendant direction
			SELECT c.*, 'D' FROM lsif_commits c WHERE c.repository_id = $1 AND c."commit" = $2
		) l

		UNION

		SELECT * FROM (
			WITH l_inner AS (SELECT * FROM lineage)
			-- get next ancestors (multiple parents for merge commits)
			SELECT c.*, 'A' FROM l_inner l JOIN lsif_commits c ON l.direction = 'A' AND c.repository_id = l.repository_id AND c."commit" = l.parent_commit
			UNION
			-- get next descendants
			SELECT c.*, 'D' FROM l_inner l JOIN lsif_commits c ON l.direction = 'D' and c.repository_id = l.repository_id AND c.parent_commit = l."commit"
		) subquery
	)
`

var visibleDumps = lineageWithDumps + `,
	visible_ids AS (
		-- Remove dumps where there exists another visible dump of smaller depth with an
		-- overlapping root from the same indexer. Such dumps would not be returned with
		-- a closest commit query so we don't want to return results for them in global
		-- find-reference queries either.
		SELECT DISTINCT t1.dump_id as id FROM lineage_with_dumps t1 WHERE NOT EXISTS (
			SELECT 1 FROM lineage_with_dumps t2
			WHERE t2.n < t1.n AND t1.indexer = t2.indexer AND (
				t2.root LIKE (t1.root || '%') OR
				t1.root LIKE (t2.root || '%')
			)
		)
	)
`

const MaxTraversalLimit = 100

var lineageWithDumps = fmt.Sprintf(`
	-- Limit the visibility to the maximum traversal depth and approximate
	-- each commit's depth by its row number.
	limited_lineage AS (
		SELECT a.*, row_number() OVER() as n from lineage a LIMIT %d
	),
	-- Correlate commits to dumps and filter out commits without LSIF data
	lineage_with_dumps AS (
		SELECT a.*, d.root, d.indexer, d.id as dump_id FROM limited_lineage a
		JOIN lsif_dumps d ON d.repository_id = a.repository_id AND d."commit" = a."commit"
	)
`, MaxTraversalLimit)

//
//
//

type Location struct {
	DumpID int    `json:"dumpId"`
	Path   string `json:"path"`
	Range  Range  `json:"range"`
}

type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type Database struct {
	bundleManagerURL string
	dumpID           int
}

func (d *Database) Exists(path string) (exists bool, err error) {
	err = d.request("exists", url.Values{"path": []string{path}}, &exists)
	return
}

func (d *Database) Definitions(path string, line, character int) ([]Location, error) {
	var locations []Location
	err := d.request("definitions", url.Values{"path": []string{path}, "line": []string{fmt.Sprintf("%d", line)}, "character": []string{fmt.Sprintf("%d", character)}}, &locations)
	if err != nil {
		return nil, err
	}
	for i := range locations {
		locations[i].DumpID = d.dumpID
	}
	return locations, nil
}

func (d *Database) References(path string, line, character int) ([]Location, error) {
	var locations []Location
	err := d.request("references", url.Values{"path": []string{path}, "line": []string{fmt.Sprintf("%d", line)}, "character": []string{fmt.Sprintf("%d", character)}}, &locations)
	if err != nil {
		return nil, err
	}
	for i := range locations {
		locations[i].DumpID = d.dumpID
	}
	return locations, nil
}

func (d *Database) Hover(path string, line, character int) (text string, r Range, exists bool, err error) {
	var target json.RawMessage
	err = d.request("hover", url.Values{"path": []string{path}, "line": []string{fmt.Sprintf("%d", line)}, "character": []string{fmt.Sprintf("%d", character)}}, &target)

	if string(target) == "null" {
		exists = false
		return
	}
	exists = true

	payload := struct {
		Text  string `json:"text"`
		Range Range  `json:"range"`
	}{}
	err = json.Unmarshal(target, &payload)
	text = payload.Text
	r = payload.Range
	return
}

type MonikerData struct {
	Kind                 string `json:"kind"`
	Scheme               string `json:"scheme"`
	Identifier           string `json:"identifier"`
	PackageInformationID string `json:"packageInformationID"`
}

func (d *Database) MonikersByPosition(path string, line, character int) (target [][]MonikerData, err error) {
	err = d.request("monikersByPosition", url.Values{"path": []string{path}, "line": []string{fmt.Sprintf("%d", line)}, "character": []string{fmt.Sprintf("%d", character)}}, &target)
	return
}

func (d *Database) MonikerResults(modelType, scheme, identifier string, skip, take *int) (locations []Location, count int, err error) {
	target := struct {
		Locations []Location `json:"locations"`
		Count     int        `json:"count"`
	}{}

	vars := url.Values{
		"modelType":  []string{modelType},
		"scheme":     []string{scheme},
		"identifier": []string{identifier},
	}
	if skip != nil {
		vars["skip"] = []string{fmt.Sprintf("%d", *skip)}
	}
	if take != nil {
		vars["take"] = []string{fmt.Sprintf("%d", *take)}
	}

	if err = d.request("monikerReults", vars, &target); err != nil {
		return
	}

	locations = target.Locations
	count = target.Count
	for i := range locations {
		locations[i].DumpID = d.dumpID
	}
	return
}

type PackageInformationData struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

func (d *Database) PackageInformation(path, packageInformationId string) (target PackageInformationData, err error) {
	err = d.request("packageInformation", url.Values{"path": []string{path}, "packageInformationId": []string{packageInformationId}}, &target)
	return
}

func (d *Database) request(path string, qs url.Values, target interface{}) error {
	url, err := url.Parse(fmt.Sprintf("%s/dbs/%d/%s", d.bundleManagerURL, d.dumpID, path))
	if err != nil {
		return err
	}
	url.RawQuery = qs.Encode()

	req, err := http.NewRequest("GET", url.String(), nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad status %d", resp.StatusCode)
	}

	return json.NewDecoder(resp.Body).Decode(&target)
}