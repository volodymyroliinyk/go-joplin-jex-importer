package main

import (
    "archive/tar"
    "bufio"
    "compress/gzip"
    "encoding/json"
    "errors"
    "flag"
    "fmt"
    "io"
    "io/fs"
    "net"
    "net/http"
    "net/url"
    "os"
    "os/exec"
    "path/filepath"
    "regexp"
    "strconv"
    "strings"
    "time"
)

// -------- Data models --------

type Note struct {
    ID              string   `json:"id"`
    Title           string   `json:"title"`
    Body            string   `json:"body"`
    ParentID        string   `json:"parent_id"`
    UserCreatedTime int64    `json:"user_created_time"`
    UserUpdatedTime int64    `json:"user_updated_time"`
    TagTitles       []string `json:"-"` // optional, from meta 'tags:' if present
}

type Resource struct {
    ID            string `json:"id"`
    Title         string `json:"title"`
    Mime          string `json:"mime"`
    FileExtension string `json:"file_extension"`
    Filename      string `json:"filename"`
    FilePath      string `json:"-"`
}

type Tag struct {
    ID    string `json:"id"`
    Title string `json:"title"`
}

type NoteTag struct {
    NoteID string `json:"note_id"`
    TagID  string `json:"tag_id"`
}

// -------- Flags / globals --------

var (
    jexPath    = flag.String("jex", "", "Path to .jex file (Joplin Export)")
    notebookID = flag.String("notebook", "", "Destination notebook ID in local Joplin")

    apiBase  = flag.String("api", "http://127.0.0.1:41184", "Joplin Data API base URL")
    apiToken = flag.String("token", "", "Joplin Web Clipper token")

    keepTimes  = flag.Bool("keep-times", true, "Preserve user_created_time / user_updated_time on upsert")
    useLinks   = flag.Bool("use-links", true, "Use explicit note<->tag links from JEX (type_: note_tag)")
    importTags = flag.Bool("tags", false, "Also attach tags from note meta 'tags:' (titles) (optional)")

    // Auto-start headless server options
    autoStart = flag.Bool("auto-start", false, "Try to auto-start Joplin CLI server if API is not reachable")
    profile   = flag.String("profile", filepath.Join(os.Getenv("HOME"), ".config", "joplin-desktop"), "Path to Joplin desktop profile (for CLI server)")
    joplinBin = flag.String("joplin-bin", "joplin", "Path to Joplin CLI binary")

    tmpDir       string
    httpClient   = &http.Client{Timeout: 60 * time.Second}
    resourceIDre = regexp.MustCompile(`!\[[^\]]*\]\(:/([0-9a-fA-F]{32})\)`) // ![name](:/resourceId) allow upper hex too
    metaKVre     = regexp.MustCompile(`^([A-Za-z0-9_]+):\s*(.*)$`)          // key: value (trailing meta lines)
    hex32re      = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// -------- Main --------

func main() {
    flag.Parse()
    if *jexPath == "" || *notebookID == "" || *apiToken == "" {
        fmt.Println("Usage:")
        flag.PrintDefaults()
        os.Exit(2)
    }
    baseURL := strings.TrimRight(*apiBase, "/")

    var err error
    tmpDir, err = os.MkdirTemp("", "jexmd-*")
    must(err)
    defer os.RemoveAll(tmpDir)

    // Ensure API server is available (optionally auto-start)
    if !isApiAlive(baseURL, *apiToken) {
        if *autoStart {
            port := extractPort(baseURL)
            fmt.Printf("• API not reachable, trying to start Joplin CLI server on port %d …\n", port)
            // Persist desired port into profile (so server listens where we expect)
            runCmd(*joplinBin, []string{"--profile", *profile, "config", "api.port", fmt.Sprint(port)})
            // Start server
            cmd := exec.Command(*joplinBin, "--profile", *profile, "server", "start")
            cmd.Stdout = os.Stdout
            cmd.Stderr = os.Stderr
            if err := cmd.Start(); err != nil {
                fmt.Fprintf(os.Stderr, "ERROR: failed to start Joplin CLI server: %v\n", err)
                os.Exit(1)
            }
            // Wait until alive
            if err := waitApi(baseURL, *apiToken, 25*time.Second); err != nil {
                fmt.Fprintf(os.Stderr, "ERROR: API did not become ready: %v\n", err)
                os.Exit(1)
            }
            defer func() { _ = runCmd(*joplinBin, []string{"--profile", *profile, "server", "stop"}) }()
        } else {
            fmt.Fprintln(os.Stderr, "ERROR: API is not reachable. Start Joplin Desktop (Web Clipper enabled) or run with -auto-start to launch CLI server.")
            os.Exit(1)
        }
    }

    fmt.Println("• Extracting JEX to temp dir…")
    must(extractJEX(*jexPath, tmpDir))

    fmt.Println("• Scanning export (Markdown + trailing meta)…")
    notes, refResIDs, tags, links, err := scanAllFromMD(tmpDir, *useLinks)
    must(err)

    // Build resource structs for the referenced IDs we actually need
    resources, err := findResources(tmpDir, refResIDs)
    must(err)

    fmt.Println("• Ensuring destination notebook exists…")
    _, err = apiGET("/folders/"+url.PathEscape(*notebookID), map[string]string{"fields": "id"})
    must(err)

    fmt.Printf("• Found: notes=%d, tags=%d, links=%d\n", len(notes), len(tags), len(links))

    // Upsert resources (build old->new id map if server reassigns)
    fmt.Printf("• Importing %d resources…\n", len(resources))
    resIDMap := map[string]string{} // old -> new
    for i, r := range resources {
        newID, err := upsertResource(r)
        if err != nil {
            fmt.Printf("  - resource %s: %v\n", r.ID, err)
            continue
        }
        resIDMap[r.ID] = newID
        if (i+1)%25 == 0 || i == len(resources)-1 {
            fmt.Printf("  %d/%d\n", i+1, len(resources))
        }
    }

    // Upsert notes. Track ID remaps.
    fmt.Printf("• Upserting %d notes into notebook %s…\n", len(notes), *notebookID)
    noteIDMap := map[string]string{} // old -> new
    for i, n := range notes {
        // Force target notebook, as requested
        n.ParentID = *notebookID

        // Rewrite resource IDs in body if any changed
        if len(resIDMap) > 0 {
            n.Body = rewriteResourceIDs(n.Body, resIDMap)
        }

        newID, err := upsertNote(n, *keepTimes)
        must(err)
        if newID != "" && newID != n.ID {
            noteIDMap[n.ID] = newID
            fmt.Printf("  • note id remap: %s → %s\n", n.ID, newID)
        }

        // Optional: attach tags from note meta titles (by title only)
        if *importTags && len(n.TagTitles) > 0 {
            realNoteID := n.ID
            if v := noteIDMap[n.ID]; v != "" {
                realNoteID = v
            }
            for _, tt := range n.TagTitles {
                tt = strings.TrimSpace(tt)
                if tt == "" {
                    continue
                }
                attachTagByTitle(tt, realNoteID)
            }
        }

        if (i+1)%50 == 0 || i == len(notes)-1 {
            fmt.Printf("  %d/%d\n", i+1, len(notes))
        }
    }

    // Build fileTagID -> title map
    fileTagByID := map[string]Tag{}
    titleSet := map[string]struct{}{}
    for _, t := range tags {
        fileTagByID[t.ID] = t
        if strings.TrimSpace(t.Title) != "" {
            titleSet[t.Title] = struct{}{}
        }
    }

    // Ensure all tag titles exist in DB; build title->dbID and fileID->dbID maps
    fmt.Printf("• Ensuring %d unique tag titles in DB (title-first strategy)…\n", len(titleSet))
    titleToDB := map[string]string{}
    for title := range titleSet {
        id, err := findTagByTitleExact(title)
        if err != nil {
            fmt.Printf("  - search tag '%s' failed: %v\n", title, err)
            continue
        }
        if id == "" {
            // create
            req := map[string]string{"title": title}
            bb, _ := json.Marshal(req)
            resp, err := apiPOST("/tags", string(bb), "application/json")
            if err != nil {
                fmt.Printf("  - create tag '%s' failed: %v\n", title, err)
                continue
            }
            var out struct {
                ID string `json:"id"`
            }
            _ = json.Unmarshal(resp, &out)
            if out.ID == "" {
                fmt.Printf("  - create tag '%s' returned empty id\n", title)
                continue
            }
            titleToDB[title] = out.ID
            fmt.Printf("  • created tag '%s' → %s\n", title, out.ID)
        } else {
            titleToDB[title] = id
            // fmt.Printf("  • found tag '%s' → %s\n", title, id)
        }
    }

    fileTagIDtoDB := map[string]string{}
    for fid, t := range fileTagByID {
        if dbid := titleToDB[t.Title]; dbid != "" {
            fileTagIDtoDB[fid] = dbid
        }
    }

    // Attach explicit note<->tag links from JEX using DB tag IDs (prefer dbExistingTagId by title)
    if *useLinks && len(links) > 0 {
        fmt.Printf("• Ensuring %d note<->tag links (prefer DB tag IDs by title)…\n", len(links))
        fail := 0
        miss := 0
        for i, lt := range links {
            // Map note id if server re-assigned
            noteID := lt.NoteID
            if v := noteIDMap[noteID]; v != "" {
                noteID = v
            }
            // Map tag id via title-first map if available
            tagID := lt.TagID
            if v := fileTagIDtoDB[tagID]; v != "" {
                tagID = v
            } else {
                // maybe link references a tag not present among tag objects; last resort: try using ID as-is
                miss++
            }

            // Avoid duplicate link if already attached
            if noteHasTag(noteID, tagID) {
                continue
            }
            if err := attachTagIDToNoteID(tagID, noteID); err != nil {
                fmt.Printf("  - link %s -> %s failed: %v\n", tagID, noteID, err)
                fail++
            }
            if (i+1)%200 == 0 || i == len(links)-1 {
                fmt.Printf("  %d/%d (fails: %d, unmapped_tag_ids: %d)\n", i+1, len(links), fail, miss)
            }
        }
    }

    fmt.Println("✔ Done: title-first tags ensured; notes & links established without duplicates.")
}

// -------- Type detection --------

func detectType(meta map[string]string) string {
    // Prefer explicit string value
    v := strings.ToLower(strings.TrimSpace(firstNonEmpty(meta["type_"], meta["type"], meta["item_type"])))
    switch v {
    case "note":
        return "note"
    case "tag":
        return "tag"
    case "note_tag", "note-tag", "notetag":
        return "note_tag"
    }
    // Numeric codes (Joplin model types): 1=note, 5=tag, 6=note_tag
    if v != "" {
        if n, err := strconv.Atoi(v); err == nil {
            switch n {
            case 1:
                return "note"
            case 5:
                return "tag"
            case 6:
                return "note_tag"
            }
        }
    }
    // Heuristic: presence of both note_id and tag_id
    if hex32re.MatchString(strings.ToLower(meta["note_id"])) && hex32re.MatchString(strings.ToLower(meta["tag_id"])) {
        return "note_tag"
    }
    // Fallback
    return "note"
}

// -------- API liveness / server helpers --------

func isApiAlive(baseURL, token string) bool {
    client := &http.Client{Timeout: 1200 * time.Millisecond}
    req, _ := http.NewRequest("GET", baseURL+"/notes?limit=1&token="+url.QueryEscape(token), nil)
    resp, err := client.Do(req)
    if err != nil {
        return false
    }
    defer resp.Body.Close()
    return resp.StatusCode == http.StatusOK
}

func waitApi(baseURL, token string, d time.Duration) error {
    deadline := time.Now().Add(d)
    for time.Now().Before(deadline) {
        if isApiAlive(baseURL, token) {
            return nil
        }
        time.Sleep(500 * time.Millisecond)
    }
    return errors.New("timeout")
}

func extractPort(baseURL string) int {
    u, err := url.Parse(baseURL)
    if err != nil {
        return 41184
    }
    _, p, err := net.SplitHostPort(u.Host)
    if err != nil {
        return 41184
    }
    var port int
    fmt.Sscanf(p, "%d", &port)
    if port == 0 {
        port = 41184
    }
    return port
}

func runCmd(bin string, args []string) error {
    cmd := exec.Command(bin, args...)
    cmd.Stdout = os.Stdout
    cmd.Stderr = os.Stderr
    return cmd.Run()
}

// -------- JEX extraction --------

func extractJEX(path, dest string) error {
    f, err := os.Open(path)
    if err != nil {
        return err
    }
    defer f.Close()

    var tr *tar.Reader
    // JEX may be plain tar or gzipped tar (most are plain tar). Try both.
    buf := make([]byte, 2)
    if _, err := f.Read(buf); err != nil {
        return err
    }
    if _, err := f.Seek(0, io.SeekStart); err != nil {
        return err
    }
    if buf[0] == 0x1f && buf[1] == 0x8b {
        gzr, err := gzip.NewReader(f)
        if err != nil {
            return err
        }
        defer gzr.Close()
        tr = tar.NewReader(gzr)
    } else {
        tr = tar.NewReader(f)
    }

    for {
        h, err := tr.Next()
        if errors.Is(err, io.EOF) {
            break
        }
        if err != nil {
            return err
        }
        target := filepath.Join(dest, filepath.FromSlash(h.Name))
        switch h.Typeflag {
        case tar.TypeDir:
            if err := os.MkdirAll(target, 0o755); err != nil {
                return err
            }
        case tar.TypeReg:
            if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
                return err
            }
            out, err := os.Create(target)
            if err != nil {
                return err
            }
            if _, err := io.Copy(out, tr); err != nil {
                out.Close()
                return err
            }
            out.Close()
        }
    }
    return nil
}

// -------- Unified scanner for notes, tags, and links --------

func scanAllFromMD(root string, readLinks bool) (notes []Note, refRes map[string]struct{}, tags []Tag, links []NoteTag, err error) {
    refRes = map[string]struct{}{}

    err = filepath.WalkDir(root, func(path string, d fs.DirEntry, e error) error {
        if e != nil || d.IsDir() {
            return e
        }
        if strings.ToLower(filepath.Ext(d.Name())) != ".md" {
            return nil
        }

        body, meta, perr := parseTrailingMeta(path)
        if perr != nil {
            // Skip non-parsable files silently
            return nil
        }

        switch detectType(meta) {
        case "tag":
            id := strings.ToLower(meta["id"])
            title := meta["title"]
            if hex32re.MatchString(id) {
                tags = append(tags, Tag{ID: id, Title: title})
            }
            return nil
        case "note_tag":
            if !readLinks {
                return nil
            }
            nid := strings.ToLower(meta["note_id"])
            tid := strings.ToLower(meta["tag_id"])
            if hex32re.MatchString(nid) && hex32re.MatchString(tid) {
                links = append(links, NoteTag{NoteID: nid, TagID: tid})
            }
            return nil
        default: // note
            id := strings.ToLower(meta["id"])
            if !hex32re.MatchString(id) {
                return nil
            }
            n := Note{
                ID:       id,
                Title:    meta["title"],
                Body:     strings.TrimRight(body, "\r\n"),
                ParentID: meta["parent_id"],
            }
            // timestamps
            if v := firstNonEmpty(meta["user_created_time"], meta["created_time"]); v != "" {
                n.UserCreatedTime = parseTimeMs(v)
            }
            if v := firstNonEmpty(meta["user_updated_time"], meta["updated_time"]); v != "" {
                n.UserUpdatedTime = parseTimeMs(v)
            }
            // If no title set, derive from first heading or filename
            if strings.TrimSpace(n.Title) == "" {
                n.Title = extractTitleFromBody(n.Body)
                if n.Title == "" {
                    n.Title = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
                }
            }
            // optional tags by title
            if tline, ok := meta["tags"]; ok && tline != "" {
                var titles []string
                if strings.HasPrefix(tline, "[") {
                    _ = json.Unmarshal([]byte(tline), &titles)
                }
                if len(titles) == 0 {
                    parts := strings.Split(tline, ",")
                    for _, p := range parts {
                        p = strings.TrimSpace(strings.Trim(p, `"'`))
                        if p != "" {
                            titles = append(titles, p)
                        }
                    }
                }
                n.TagTitles = titles
            }
            // referenced resources
            matches := resourceIDre.FindAllStringSubmatch(n.Body, -1)
            for _, m := range matches {
                if len(m) == 2 {
                    id := strings.ToLower(m[1])
                    if hex32re.MatchString(id) {
                        refRes[id] = struct{}{}
                    }
                }
            }
            notes = append(notes, n)
            return nil
        }
    })
    return
}

// parseTrailingMeta: read file and split into (body, meta map).
// Meta block is detected by scanning from bottom lines with "key: value".
// Consider it valid only if it contains 32-hex "id".
func parseTrailingMeta(path string) (string, map[string]string, error) {
    raw, err := os.ReadFile(path)
    if err != nil {
        return "", nil, err
    }
    text := string(raw)

    lines := splitLines(text)
    i := len(lines) - 1
    meta := map[string]string{}
    seenKV := false

    for i >= 0 {
        line := strings.TrimRight(lines[i], "\r\n")
        if line == "" {
            if seenKV {
                i--
                break
            }
            i--
            continue
        }
        if kv := metaKVre.FindStringSubmatch(line); kv != nil {
            key := strings.ToLower(kv[1])
            val := strings.TrimSpace(kv[2])
            if _, exists := meta[key]; !exists {
                meta[key] = val
            }
            seenKV = true
            i--
            continue
        }
        break
    }

    // For tags and note_tag, there's also an 'id'. Keep the guard to avoid false-positives.
    if id, ok := meta["id"]; !ok || !hex32re.MatchString(strings.ToLower(id)) {
        return "", nil, errors.New("no valid trailing meta id")
    }

    body := strings.Join(lines[:i+1], "")
    return body, meta, nil
}

// -------- Resource discovery --------

func findResources(root string, refIDs map[string]struct{}) ([]Resource, error) {
    var out []Resource
    resDir := filepath.Join(root, "resources")

    // If no resources dir, nothing to do
    if fi, err := os.Stat(resDir); err != nil || !fi.IsDir() {
        return out, nil
    }

    err := filepath.WalkDir(resDir, func(path string, d fs.DirEntry, e error) error {
        if e != nil || d.IsDir() {
            return e
        }
        base := filepath.Base(path)
        // Try detect id from filename start
        id := ""
        ext := strings.ToLower(filepath.Ext(base))
        nameNoExt := strings.TrimSuffix(base, ext)
        if hex32re.MatchString(nameNoExt) {
            id = nameNoExt
        } else if len(base) >= 32 && hex32re.MatchString(strings.ToLower(base[:32])) {
            id = strings.ToLower(base[:32])
        }

        // Only include if referenced by some note
        if id == "" {
            return nil
        }
        if _, needed := refIDs[id]; !needed {
            return nil
        }

        r := Resource{
            ID:            id,
            Title:         base,
            FileExtension: ext,
            Filename:      base,
            FilePath:      path,
        }
        out = append(out, r)
        return nil
    })
    return out, err
}

// -------- API helpers / upsert --------

func apiURL(path string, q map[string]string) string {
    u, _ := url.Parse(*apiBase + path)
    qv := u.Query()
    qv.Add("token", *apiToken)
    for k, v := range q {
        qv.Set(k, v)
    }
    u.RawQuery = qv.Encode()
    return u.String()
}

func apiGET(path string, q map[string]string) ([]byte, error) {
    resp, err := httpClient.Get(apiURL(path, q))
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()
    if resp.StatusCode >= 400 {
        b, _ := io.ReadAll(resp.Body)
        return nil, fmt.Errorf("GET %s: %s - %s", path, resp.Status, string(b))
    }
    return io.ReadAll(resp.Body)
}

func apiPOST(path string, body string, contentType string) ([]byte, error) {
    if contentType == "" {
        contentType = "application/json"
    }
    resp, err := httpClient.Post(apiURL(path, nil), contentType, strings.NewReader(body))
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()
    if resp.StatusCode >= 400 {
        b, _ := io.ReadAll(resp.Body)
        return nil, fmt.Errorf("POST %s: %s - %s", path, resp.Status, string(b))
    }
    return io.ReadAll(resp.Body)
}

func apiPUT(path string, body string, contentType string) ([]byte, error) {
    if contentType == "" {
        contentType = "application/json"
    }
    req, _ := http.NewRequest(http.MethodPut, apiURL(path, nil), strings.NewReader(body))
    req.Header.Set("Content-Type", contentType)
    resp, err := httpClient.Do(req)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()
    if resp.StatusCode >= 400 {
        b, _ := io.ReadAll(resp.Body)
        return nil, fmt.Errorf("PUT %s: %s - %s", path, resp.Status, string(b))
    }
    return io.ReadAll(resp.Body)
}

func upsertNote(n Note, keepTimes bool) (string, error) {
    // Does it exist?
    if _, err := apiGET("/notes/"+url.PathEscape(n.ID), map[string]string{"fields": "id"}); err == nil {
        // update
        payload := map[string]any{
            "title":     n.Title,
            "parent_id": n.ParentID,
            "body":      n.Body,
        }
        if keepTimes {
            if n.UserCreatedTime > 0 {
                payload["user_created_time"] = n.UserCreatedTime
            }
            if n.UserUpdatedTime > 0 {
                payload["user_updated_time"] = n.UserUpdatedTime
            }
        }
        b, _ := json.Marshal(payload)
        _, err = apiPUT("/notes/"+url.PathEscape(n.ID), string(b), "application/json")
        return n.ID, err
    }
    // create
    payload := map[string]any{
        "id":        n.ID,
        "title":     n.Title,
        "parent_id": n.ParentID,
        "body":      n.Body,
    }
    if keepTimes {
        if n.UserCreatedTime > 0 {
            payload["user_created_time"] = n.UserCreatedTime
        }
        if n.UserUpdatedTime > 0 {
            payload["user_updated_time"] = n.UserUpdatedTime
        }
    }
    b, _ := json.Marshal(payload)
    resp, err := apiPOST("/notes", string(b), "application/json")
    if err != nil {
        return n.ID, err
    }
    var out struct {
        ID string `json:"id"`
    }
    _ = json.Unmarshal(resp, &out)
    if out.ID != "" {
        return out.ID, nil
    }
    return n.ID, nil
}

// upsertResource: upload/ensure resource and return actual server ID (may differ from provided)
func upsertResource(r Resource) (string, error) {
    if r.FilePath == "" {
        return r.ID, nil
    }
    // Build multipart body manually: part "data" = file, part "props" = JSON
    props := map[string]any{"id": r.ID, "title": r.Title}
    if r.FileExtension != "" {
        props["file_extension"] = strings.TrimPrefix(r.FileExtension, ".")
    }
    propsJSON, _ := json.Marshal(props)

    body := &strings.Builder{}
    w := multipartWriter(body, r.FilePath, string(propsJSON))
    mbody, ctype := w.Body(), w.ContentType()

    // Try POST first
    resp, err := apiPOST("/resources", mbody, ctype)
    if err == nil {
        var out struct {
            ID string `json:"id"`
        }
        _ = json.Unmarshal(resp, &out)
        if out.ID != "" {
            return out.ID, nil
        }
        return r.ID, nil
    }
    // PUT /resources/:id
    resp, err = apiPUT("/resources/"+url.PathEscape(r.ID), mbody, ctype)
    if err == nil {
        var out struct {
            ID string `json:"id"`
        }
        _ = json.Unmarshal(resp, &out)
        if out.ID == "" {
            out.ID = r.ID
        }
        return out.ID, nil
    }
    // Last resort: create without explicit id
    props2 := map[string]any{}
    propsJSON2, _ := json.Marshal(props2)
    body2 := &strings.Builder{}
    w2 := multipartWriter(body2, r.FilePath, string(propsJSON2))
    if resp2, err2 := apiPOST("/resources", w2.Body(), w2.ContentType()); err2 == nil {
        var out struct {
            ID string `json:"id"`
        }
        _ = json.Unmarshal(resp2, &out)
        if out.ID != "" {
            return out.ID, nil
        }
    }
    return r.ID, err
}

// Minimal multipart builder

type mpw struct {
    sb       *strings.Builder
    boundary string
}

func multipartWriter(sb *strings.Builder, filePath string, props string) *mpw {
    w := &mpw{sb: sb, boundary: "----JoplinGoBoundary"}
    fname := filepath.Base(filePath)

    var b = w.boundary
    fmt.Fprintf(sb, "--%s\r\n", b)
    fmt.Fprintf(sb, "Content-Disposition: form-data; name=\"data\"; filename=\"%s\"\r\n", fname)
    fmt.Fprintf(sb, "Content-Type: application/octet-stream\r\n\r\n")
    f, _ := os.Open(filePath)
    defer f.Close()
    io.Copy(sb, f)
    fmt.Fprintf(sb, "\r\n--%s\r\n", b)
    fmt.Fprintf(sb, "Content-Disposition: form-data; name=\"props\"\r\n\r\n")
    fmt.Fprintf(sb, "%s\r\n", props)
    fmt.Fprintf(sb, "--%s--\r\n", b)
    return w
}
func (w *mpw) ContentType() string { return "multipart/form-data; boundary=" + w.boundary }
func (w *mpw) Body() string        { return w.sb.String() }

// -------- Tags: search by title / attach --------

func findTagByTitleExact(title string) (string, error) {
    if strings.TrimSpace(title) == "" {
        return "", nil
    }
    b, err := apiGET("/search", map[string]string{"query": title, "type": "tag", "limit": "100"})
    if err != nil {
        return "", err
    }
    var s struct {
        Items []struct {
            ID    string `json:"id"`
            Title string `json:"title"`
        } `json:"items"`
    }
    if err := json.Unmarshal(b, &s); err != nil {
        return "", err
    }
    for _, it := range s.Items {
        if strings.EqualFold(it.Title, title) {
            return it.ID, nil
        }
    }
    return "", nil
}

func attachTagByTitle(title, noteID string) {
    title = strings.TrimSpace(title)
    if title == "" {
        return
    }
    // Try to find tag by title via /search
    if id, err := findTagByTitleExact(title); err == nil && id != "" {
        _ = attachTagIDToNoteID(id, noteID)
        return
    }
    // Create and attach
    req := map[string]string{"title": title}
    if bb, err := json.Marshal(req); err == nil {
        if resp, err2 := apiPOST("/tags", string(bb), "application/json"); err2 == nil {
            var out struct {
                ID string `json:"id"`
            }
            _ = json.Unmarshal(resp, &out)
            if out.ID != "" {
                _ = attachTagIDToNoteID(out.ID, noteID)
            }
        }
    }
}

func attachTagIDToNoteID(tagID, noteID string) error {
    payload := map[string]string{"id": noteID}
    b, _ := json.Marshal(payload)
    _, err := apiPOST("/tags/"+url.PathEscape(tagID)+"/notes", string(b), "application/json")
    return err
}

func noteHasTag(noteID, tagID string) bool {
    b, err := apiGET("/notes/"+url.PathEscape(noteID)+"/tags", map[string]string{"fields": "id", "limit": "100"})
    if err != nil {
        return false
    }
    var s struct {
        Items []Tag `json:"items"`
    }
    _ = json.Unmarshal(b, &s)
    for _, t := range s.Items {
        if t.ID == tagID {
            return true
        }
    }
    return false
}

// -------- Small helpers --------

func splitLines(s string) []string {
    sc := bufio.NewScanner(strings.NewReader(s))
    // Preserve line endings by appending \n manually
    var lines []string
    for sc.Scan() {
        lines = append(lines, sc.Text()+"\n")
    }
    return lines
}

func extractTitleFromBody(body string) string {
    for _, line := range strings.Split(body, "\n") {
        trim := strings.TrimSpace(line)
        if strings.HasPrefix(trim, "# ") {
            return strings.TrimSpace(strings.TrimPrefix(trim, "# "))
        }
    }
    return ""
}

func firstNonEmpty(vals ...string) string {
    for _, v := range vals {
        if strings.TrimSpace(v) != "" {
            return v
        }
    }
    return ""
}

func parseTimeMs(s string) int64 {
    // Parse decimal integer; allow seconds or milliseconds
    s = strings.TrimSpace(s)
    if s == "" {
        return 0
    }
    var x int64
    for i := 0; i < len(s); i++ {
        if s[i] < '0' || s[i] > '9' {
            return 0
        }
    }
    _, _ = fmt.Sscanf(s, "%d", &x)
    if x < 1e12 {
        return x * 1000
    }
    return x
}

func rewriteResourceIDs(md string, idmap map[string]string) string {
    return resourceIDre.ReplaceAllStringFunc(md, func(m string) string {
        sub := resourceIDre.FindStringSubmatch(m)
        if len(sub) < 2 {
            return m
        }
        old := strings.ToLower(sub[1])
        newID := idmap[old]
        if newID == "" || newID == old {
            return m
        }
        return strings.Replace(m, sub[1], newID, 1)
    })
}

func must(err error) {
    if err != nil {
        fmt.Fprintln(os.Stderr, "Error:", err)
        os.Exit(1)
    }
}
