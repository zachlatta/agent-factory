package factory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf16"
)

var errScheduleEvidence = errors.New("schedule evidence unavailable, conflicting or unsupported; preserve resources")
var scheduleHex = regexp.MustCompile(`^[0-9a-f]{64}$`)
var scheduleHead = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
var scheduleInteger = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

// Standalone consumer support only: no command, inventory or executor calls this.
// Inputs other than Manifest must originate from a separately reviewed owner adapter.
type scheduleRequest struct {
	Receipt, Declaration, Outcome, Identity, Manifest []byte
	Release                                           scheduleRelease
}
type schedulePolicy struct {
	Store  string
	MaxAge time.Duration
}

// scheduleRelease is an internal projection of v1's release requirements, NOT a
// scheduler wire format. Only an authoritative adapter may construct trusted values.
type scheduleRelease struct {
	SchedulerID, ScheduleID, RunID, AgentID, WorkspaceID, CanonicalPath string
	CommonDir, Branch, Head                                             string
	Generation, ReceiptDigest, DeclarationProvenance                    string
	ReleasedAt, ExpiresAt                                               time.Time
}

// The future adapter must hold authoritative release/exclusion through the callback,
// verify revocation, declaration approval and complete scheduler-reference coverage.
// A boolean supplied by a run agent is never such an adapter.
type scheduleOwner interface {
	withRelease(context.Context, scheduleRelease, func() error) error
}
type scheduleUnavailableOwner struct{}

func (scheduleUnavailableOwner) withRelease(context.Context, scheduleRelease, func() error) error {
	return errScheduleEvidence
}
func schedulePolicyFromEnv(get func(string) string) (schedulePolicy, error) {
	age, err := time.ParseDuration(get("FACTORY_SCHEDULE_RECEIPT_MAX_AGE"))
	p := schedulePolicy{get("FACTORY_SCHEDULE_ACK_STORE"), age}
	if err != nil || age <= 0 || scheduleStore(p.Store) != nil {
		return schedulePolicy{}, errScheduleEvidence
	}
	return p, nil
}
func scheduleHash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func scheduleText(v any) string {
	s, _ := v.(string)
	if strings.TrimSpace(s) == "" || strings.ContainsRune(s, 0) {
		return ""
	}
	return s
}
func scheduleExact(m map[string]any, keys string) bool {
	names := strings.Fields(keys)
	if len(m) != len(names) {
		return false
	}
	for _, k := range names {
		if _, ok := m[k]; !ok {
			return false
		}
	}
	return true
}
func scheduleTime(v any) time.Time { t, _ := time.Parse(time.RFC3339Nano, scheduleText(v)); return t }
func scheduleCanonicalPath(p string) bool {
	q, err := filepath.EvalSymlinks(p)
	return err == nil && filepath.IsAbs(p) && q == p && filepath.Clean(p) == p
}
func scheduleStore(p string) error {
	st, err := os.Lstat(p)
	if err != nil || !scheduleCanonicalPath(p) || !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return errScheduleEvidence
	}
	owner, ok := st.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Getuid()) {
		return errScheduleEvidence
	}
	return nil
}

// Reject duplicate keys at every depth, trailing data, invalid Unicode and numeric
// spellings not produced by this schema. json.Unmarshal alone accepts duplicates.
func scheduleObject(b []byte) (map[string]any, error) {
	if !json.Valid(b) || !bytes.Equal(bytes.ToValidUTF8(b, nil), b) {
		return nil, errScheduleEvidence
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var read func() (any, error)
	read = func() (any, error) {
		tok, err := d.Token()
		if err != nil {
			return nil, errScheduleEvidence
		}
		switch v := tok.(type) {
		case json.Delim:
			if v == '{' {
				m := map[string]any{}
				for d.More() {
					k, e := d.Token()
					if e != nil {
						return nil, errScheduleEvidence
					}
					key, ok := k.(string)
					if !ok || strings.ContainsRune(key, unicode.ReplacementChar) {
						return nil, errScheduleEvidence
					}
					if _, exists := m[key]; exists {
						return nil, errScheduleEvidence
					}
					item, e := read()
					if e != nil {
						return nil, e
					}
					m[key] = item
				}
				_, err = d.Token()
				return m, err
			}
			if v == '[' {
				a := []any{}
				for d.More() {
					item, e := read()
					if e != nil {
						return nil, e
					}
					a = append(a, item)
				}
				_, err = d.Token()
				return a, err
			}
			return nil, errScheduleEvidence
		case string:
			if strings.ContainsRune(v, unicode.ReplacementChar) {
				return nil, errScheduleEvidence
			}
		case json.Number:
			if !scheduleInteger.MatchString(string(v)) {
				return nil, errScheduleEvidence
			}
		}
		return tok, nil
	}
	v, err := read()
	if err != nil {
		return nil, errScheduleEvidence
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, errScheduleEvidence
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errScheduleEvidence
	}
	return m, nil
}

// Match the producer's sorted compact JSON with ensure_ascii=True (including
// surrogate pairs). Go's default JSON encoding escapes HTML and emits UTF-8.
func scheduleCanonical(v any) []byte {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := []string{}
		for _, k := range keys {
			parts = append(parts, string(scheduleCanonical(k))+":"+string(scheduleCanonical(x[k])))
		}
		return []byte("{" + strings.Join(parts, ",") + "}")
	case []any:
		parts := []string{}
		for _, item := range x {
			parts = append(parts, string(scheduleCanonical(item)))
		}
		return []byte("[" + strings.Join(parts, ",") + "]")
	case string:
		var b strings.Builder
		b.WriteByte('"')
		for _, r := range x {
			if r >= 0x7f {
				for _, u := range utf16.Encode([]rune{r}) {
					s := strconv.FormatUint(uint64(u), 16)
					b.WriteString(`\u` + strings.Repeat("0", 4-len(s)) + s)
				}
			} else {
				q, _ := json.Marshal(string(r))
				b.Write(bytes.ReplaceAll(bytes.ReplaceAll(bytes.ReplaceAll(q[1:len(q)-1], []byte(`\u003c`), []byte("<")), []byte(`\u003e`), []byte(">")), []byte(`\u0026`), []byte("&")))
			}
		}
		b.WriteByte('"')
		return []byte(b.String())
	default:
		b, _ := json.Marshal(v)
		return b
	}
}

func validateScheduleEvidence(ctx context.Context, p schedulePolicy, q scheduleRequest, now time.Time) error {
	if now.IsZero() || p.MaxAge <= 0 || scheduleStore(p.Store) != nil {
		return errScheduleEvidence
	}
	values := []map[string]any{}
	for _, b := range [][]byte{q.Receipt, q.Declaration, q.Outcome, q.Identity, q.Manifest} {
		m, err := scheduleObject(b)
		if err != nil {
			return errScheduleEvidence
		}
		values = append(values, m)
	}
	r, d, o, id, m := values[0], values[1], values[2], values[3], values[4]
	if !scheduleExact(r, "version recordKind schedulerId scheduleId runId agentId workspaceId canonicalPath commonDir branch head resultKind outcome declarationDigest manifestDigest verifiedAt evidence retirementAuthorized") || !scheduleExact(d, "version schedulerId scheduleId resultKind requirements") || !scheduleExact(o, "status startedAt endedAt interrupted canceled pendingPermission unresolved") || !scheduleExact(id, "runId agentId workspaceId canonicalPath commonDir branch head") {
		return errScheduleEvidence
	}
	if r["version"] != json.Number("1") || d["version"] != json.Number("1") || r["recordKind"] != "success" || r["retirementAuthorized"] != false || o["status"] != "succeeded" {
		return errScheduleEvidence
	}
	for _, k := range []string{"interrupted", "canceled", "pendingPermission", "unresolved"} {
		if o[k] != false {
			return errScheduleEvidence
		}
	}
	start, end, verified := scheduleTime(o["startedAt"]), scheduleTime(o["endedAt"]), scheduleTime(r["verifiedAt"])
	if start.IsZero() || end.Before(start) || verified.Before(end) || verified.After(now) || !now.Before(verified.Add(p.MaxAge)) || !reflect.DeepEqual(r["outcome"], o) {
		return errScheduleEvidence
	}
	for _, k := range []string{"schedulerId", "scheduleId", "resultKind"} {
		if scheduleText(d[k]) == "" || r[k] != d[k] {
			return errScheduleEvidence
		}
	}
	for k, v := range id {
		if !reflect.DeepEqual(r[k], v) {
			return errScheduleEvidence
		}
	}
	for _, k := range []string{"runId", "agentId", "workspaceId", "canonicalPath"} {
		if scheduleText(id[k]) == "" {
			return errScheduleEvidence
		}
	}
	if r["declarationDigest"] != scheduleHash(scheduleCanonical(d)) || r["manifestDigest"] != scheduleHash(scheduleCanonical(m)) {
		return errScheduleEvidence
	}
	path := scheduleText(id["canonicalPath"])
	if !scheduleCanonicalPath(path) || retirementOverlap(p.Store, path) {
		return errScheduleEvidence
	}
	if common := scheduleText(id["commonDir"]); common != "" && retirementOverlap(p.Store, common) {
		return errScheduleEvidence
	}
	if err := scheduleIdentity(ctx, id); err != nil {
		return err
	}
	rel := q.Release
	for _, s := range []string{rel.Generation, rel.DeclarationProvenance} {
		if scheduleText(s) == "" {
			return errScheduleEvidence
		}
	}
	if rel.SchedulerID != d["schedulerId"] || rel.ScheduleID != d["scheduleId"] || rel.ReceiptDigest != scheduleHash(q.Receipt) || rel.ReleasedAt.Before(verified) || rel.ReleasedAt.After(now) || !now.Before(rel.ExpiresAt) {
		return errScheduleEvidence
	}
	for k, v := range map[string]string{"runId": rel.RunID, "agentId": rel.AgentID, "workspaceId": rel.WorkspaceID, "canonicalPath": rel.CanonicalPath, "commonDir": rel.CommonDir, "branch": rel.Branch, "head": rel.Head} {
		if (id[k] != nil || v != "") && id[k] != v {
			return errScheduleEvidence
		}
	}
	requirements, ok := d["requirements"].(map[string]any)
	if !ok {
		return errScheduleEvidence
	}
	evidence, ok := r["evidence"].(map[string]any)
	if !ok {
		return errScheduleEvidence
	}
	switch d["resultKind"] {
	case "commit":
		if !scheduleExact(requirements, "remote landingRef") || scheduleText(requirements["remote"]) == "" || scheduleText(requirements["landingRef"]) == "" || !scheduleExact(m, "head") || m["head"] != id["head"] || !scheduleExact(evidence, "kind remote landingRef fetchedHead") || evidence["kind"] != "commit" || !scheduleHead.MatchString(scheduleText(evidence["fetchedHead"])) || evidence["remote"] != requirements["remote"] || evidence["landingRef"] != requirements["landingRef"] {
			return errScheduleEvidence
		}
		if err := scheduleFetch(ctx, p.Store, requirements, scheduleText(id["head"])); err != nil {
			return err
		}
	case "durable-output":
		if !scheduleExact(requirements, "outputs") || !scheduleExact(m, "outputs") || !scheduleExact(evidence, "kind outputs") || evidence["kind"] != "durable-output" {
			return errScheduleEvidence
		}
		specs, a := requirements["outputs"].([]any)
		manifests, b := m["outputs"].([]any)
		outputs, c := evidence["outputs"].([]any)
		if !a || !b || !c || len(specs) == 0 || len(specs) != len(manifests) || len(specs) != len(outputs) {
			return errScheduleEvidence
		}
		seen := map[string]bool{}
		for n, s := range specs {
			spec, a := s.(map[string]any)
			manifest, b := manifests[n].(map[string]any)
			output, c := outputs[n].(map[string]any)
			if !a || !b || !c || !scheduleExact(spec, "name path") || !scheduleExact(manifest, "name sha256") || !scheduleExact(output, "name path sha256 size") {
				return errScheduleEvidence
			}
			name, file, digest := scheduleText(spec["name"]), scheduleText(spec["path"]), scheduleText(manifest["sha256"])
			if name == "" || seen[name] || manifest["name"] != name || output["name"] != name || output["path"] != file || output["sha256"] != digest || !scheduleHex.MatchString(digest) || !scheduleCanonicalPath(file) || retirementOverlap(file, path) {
				return errScheduleEvidence
			}
			seen[name] = true
			if err := scheduleOutput(file, digest, output["size"]); err != nil {
				return err
			}
		}
	default:
		return errScheduleEvidence
	}
	return scheduleIdentity(ctx, id)
}
func scheduleIdentity(ctx context.Context, id map[string]any) error {
	path := scheduleText(id["canonicalPath"])
	st, err := os.Stat(path)
	if err != nil || !st.IsDir() {
		return errScheduleEvidence
	}
	if id["commonDir"] == nil && id["branch"] == nil && id["head"] == nil {
		for p := path; ; p = filepath.Dir(p) {
			if _, err := os.Lstat(filepath.Join(p, ".git")); !os.IsNotExist(err) {
				return errScheduleEvidence
			}
			if filepath.Dir(p) == p {
				break
			}
		}
		return nil
	}
	common, branch, head := scheduleText(id["commonDir"]), scheduleText(id["branch"]), scheduleText(id["head"])
	if !scheduleCanonicalPath(common) || branch == "" || !scheduleHead.MatchString(head) {
		return errScheduleEvidence
	}
	for _, check := range []struct {
		want string
		args []string
	}{{common, []string{"rev-parse", "--path-format=absolute", "--git-common-dir"}}, {branch, []string{"symbolic-ref", "--short", "HEAD"}}, {head, []string{"rev-parse", "HEAD"}}} {
		got, err := scheduleGit(ctx, path, check.args...)
		if err != nil || got != check.want {
			return errScheduleEvidence
		}
	}
	return nil
}
func scheduleGit(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "protocol.ext.allow=never", "-C", dir}, args...)...)
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "GIT_") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	b, err := cmd.Output()
	if err != nil {
		return "", errScheduleEvidence
	}
	return strings.TrimSpace(string(b)), nil
}
func scheduleFetch(ctx context.Context, store string, req map[string]any, head string) error {
	remote, ref := scheduleText(req["remote"]), scheduleText(req["landingRef"])
	if remote == "" || strings.HasPrefix(remote, "-") || !strings.HasPrefix(ref, "refs/heads/") || !scheduleHead.MatchString(head) {
		return errScheduleEvidence
	}
	u, parseErr := url.Parse(remote)
	if !scheduleCanonicalPath(remote) && (parseErr != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "") {
		return errScheduleEvidence
	}
	dir, err := os.MkdirTemp(store, ".proof-")
	if err != nil {
		return errScheduleEvidence
	}
	defer os.RemoveAll(dir)
	for _, args := range [][]string{{"check-ref-format", ref}, {"init", "--bare", "--quiet"}, {"fetch", "--quiet", "--no-tags", "--no-recurse-submodules", "--", remote, ref}, {"merge-base", "--is-ancestor", head, "FETCH_HEAD"}} {
		if _, err := scheduleGit(ctx, dir, args...); err != nil {
			return err
		}
	}
	return nil
}
func scheduleOutput(path, digest string, size any) error {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return errScheduleEvidence
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() {
		return errScheduleEvidence
	}
	hash := sha256.New()
	n, err := io.Copy(hash, f)
	if err != nil {
		return errScheduleEvidence
	}
	after, err := f.Stat()
	current, e := os.Lstat(path)
	if err != nil || e != nil || !os.SameFile(before, current) || !scheduleStableFile(before, after) || !scheduleStableFile(after, current) || size != json.Number(strconv.FormatInt(n, 10)) || hex.EncodeToString(hash.Sum(nil)) != digest {
		return errScheduleEvidence
	}
	return nil
}

// Acceptance grants no permit and performs no resource mutation. The only
// production owner is unavailable; there are deliberately no production call sites.
func acknowledgeSchedule(ctx context.Context, p schedulePolicy, q scheduleRequest, now time.Time, owner scheduleOwner) error {
	if owner == nil {
		return errScheduleEvidence
	}
	accepted := false
	err := owner.withRelease(ctx, q.Release, func() error {
		if err := validateScheduleEvidence(ctx, p, q, now); err != nil {
			return err
		}
		err := scheduleClaim(p, q.Release, now)
		accepted = err == nil
		return err
	})
	if err != nil || !accepted {
		return errScheduleEvidence
	}
	return nil
}

// A single private store serializes claims across processes. Claims are retained
// after expiry: neither elapsed time nor a lost reply automatically releases ownership.
type scheduleClaimRecord struct {
	Version        int
	Release        scheduleRelease
	AcknowledgedAt time.Time
}

func scheduleClaim(p schedulePolicy, r scheduleRelease, now time.Time) error {
	if scheduleStore(p.Store) != nil || !scheduleReleaseShape(r) || now.IsZero() || r.ReleasedAt.After(now) || !now.Before(r.ExpiresAt) {
		return errScheduleEvidence
	}
	lock, err := os.OpenFile(filepath.Join(p.Store, ".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return errScheduleEvidence
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return errScheduleEvidence
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	entries, err := os.ReadDir(p.Store)
	if err != nil {
		return errScheduleEvidence
	}
	for _, entry := range entries {
		if entry.Name() == ".lock" {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".json") || !scheduleHex.MatchString(strings.TrimSuffix(entry.Name(), ".json")) {
			return errScheduleEvidence
		}
		path := filepath.Join(p.Store, entry.Name())
		info, e := os.Lstat(path)
		if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return errScheduleEvidence
		}
		data, e := os.ReadFile(path)
		if e != nil {
			return errScheduleEvidence
		}
		object, e := scheduleObject(data)
		release, ok := object["Release"].(map[string]any)
		if e != nil || !scheduleExact(object, "Version Release AcknowledgedAt") || object["Version"] != json.Number("1") || !ok || !scheduleExact(release, "SchedulerID ScheduleID RunID AgentID WorkspaceID CanonicalPath CommonDir Branch Head Generation ReceiptDigest DeclarationProvenance ReleasedAt ExpiresAt") {
			return errScheduleEvidence
		}
		var record scheduleClaimRecord
		if json.Unmarshal(data, &record) != nil || !scheduleReleaseShape(record.Release) || record.AcknowledgedAt.IsZero() || record.AcknowledgedAt.Before(record.Release.ReleasedAt) || !record.AcknowledgedAt.Before(record.Release.ExpiresAt) {
			return errScheduleEvidence
		}
		old := record.Release
		if entry.Name() != scheduleHash(scheduleCanonical([]any{old.SchedulerID, old.ScheduleID, old.RunID}))+".json" {
			return errScheduleEvidence
		}
		if old.WorkspaceID == r.WorkspaceID || old.AgentID == r.AgentID || (old.SchedulerID == r.SchedulerID && old.ScheduleID == r.ScheduleID && old.RunID == r.RunID) || retirementOverlap(old.CanonicalPath, r.CanonicalPath) || (old.CommonDir != "" && old.CommonDir == r.CommonDir && old.Branch == r.Branch) {
			return errScheduleEvidence
		}
	}
	key := scheduleHash(scheduleCanonical([]any{r.SchedulerID, r.ScheduleID, r.RunID}))
	// fsync and link without replacement. If publication succeeds but durability
	// confirmation fails, keep the claim and refuse; reconciliation must inspect it.
	stage, err := os.CreateTemp(p.Store, ".claim-")
	if err != nil {
		return errScheduleEvidence
	}
	defer os.Remove(stage.Name())
	defer stage.Close()
	b, err := json.Marshal(scheduleClaimRecord{1, r, now})
	if err != nil {
		return errScheduleEvidence
	}
	if _, err = stage.Write(b); err != nil {
		return errScheduleEvidence
	}
	if stage.Sync() != nil {
		return errScheduleEvidence
	}
	if os.Link(stage.Name(), filepath.Join(p.Store, key+".json")) != nil {
		return errScheduleEvidence
	}
	if os.Remove(stage.Name()) != nil {
		return errScheduleEvidence
	}
	dir, err := os.Open(p.Store)
	if err != nil {
		return errScheduleEvidence
	}
	defer dir.Close()
	if dir.Sync() != nil {
		return errScheduleEvidence
	}
	return nil
}

// Access time can advance during our read; identity, mode and change/modification
// times must remain stable. Field names cover the supported Unix stat layouts.
func scheduleStableFile(a, b os.FileInfo) bool {
	left, right := reflect.ValueOf(a.Sys()).Elem(), reflect.ValueOf(b.Sys()).Elem()
	for _, field := range []string{"Dev", "Ino", "Size", "Mode", "Nlink", "Mtim", "Ctim", "Mtimespec", "Ctimespec"} {
		x, y := left.FieldByName(field), right.FieldByName(field)
		if x.IsValid() && (!y.IsValid() || !reflect.DeepEqual(x.Interface(), y.Interface())) {
			return false
		}
	}
	return true
}

func scheduleReleaseShape(r scheduleRelease) bool {
	for _, s := range []string{r.SchedulerID, r.ScheduleID, r.RunID, r.AgentID, r.WorkspaceID, r.Generation, r.DeclarationProvenance} {
		if scheduleText(s) == "" {
			return false
		}
	}
	return filepath.IsAbs(r.CanonicalPath) && filepath.Clean(r.CanonicalPath) == r.CanonicalPath && scheduleHex.MatchString(r.ReceiptDigest) && !r.ReleasedAt.IsZero() && r.ExpiresAt.After(r.ReleasedAt) && ((r.CommonDir == "" && r.Branch == "" && r.Head == "") || (filepath.IsAbs(r.CommonDir) && r.Branch != "" && scheduleHead.MatchString(r.Head)))
}
