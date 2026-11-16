package domain

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Private request attachments (contract artifact 36 v2 §1, build slices
// G1/G3/G4): the bounds the approved mockups state, the scan states that must
// keep "unknown" apart from "clean", the restricted-file hold, and the 90-day
// retention with a team keep. Nothing here proves storage, upload, download or
// a purge job — those are G2/G3/G4's own tests; this file proves the domain
// contract they must satisfy.

func validAttachment() *RequestAttachment {
	return &RequestAttachment{
		ID:          41,
		RequestID:   7,
		FileName:    "idp-metadata.xml",
		ContentType: "text/xml",
		SizeBytes:   41 << 10,
		SHA256:      strings.Repeat("a1", 32),
		ScanState:   RequestScanClean,
		UploadedBy:  ptrInt(31),
		CreatedAt:   time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC),
	}
}

// requestTime is a fixed clock so a retention test never depends on today.
func requestTime(y int, mo time.Month, d int) time.Time {
	return time.Date(y, mo, d, 12, 0, 0, 0, time.UTC)
}

// The four scan states are the contract's, and nothing else is one. "Unknown"
// and "clean" are separate states, and only clean may be called scanned.
func TestRequestScanStates(t *testing.T) {
	want := []string{"pending", "clean", "infected", "unsupported"}
	if got := len(RequestScanStates); got != len(want) {
		t.Fatalf("RequestScanStates has %d values, want the approved four: %v", got, RequestScanStates)
	}
	for i, w := range want {
		if string(RequestScanStates[i]) != w {
			t.Errorf("RequestScanStates[%d] = %q, want %q", i, RequestScanStates[i], w)
		}
		if got, err := NormalizeRequestScanState(w); err != nil || string(got) != w {
			t.Errorf("NormalizeRequestScanState(%q) = %q, %v", w, got, err)
		}
	}
	for _, bad := range []string{"scanned", "quarantined", "failed", "pending_scan", "unknown", "dirty"} {
		if got, err := NormalizeRequestScanState(bad); err == nil {
			t.Errorf("NormalizeRequestScanState(%q) = %q, want an error naming the four states", bad, got)
		} else if !strings.Contains(err.Error(), "unsupported") {
			t.Errorf("NormalizeRequestScanState(%q) error = %q, want it to name the four states", bad, err)
		}
	}
}

// No verdict is the truthful default: AMP has no scanner, so a missing state
// must never read as clean (a false safety claim) or as pending (a permanent
// hold behind a scanner that never runs).
func TestRequestScanStateDefaultsToUnknownNotClean(t *testing.T) {
	for _, in := range []string{"", "   ", "\t"} {
		got, err := NormalizeRequestScanState(in)
		if err != nil {
			t.Fatalf("NormalizeRequestScanState(%q) = %v, want the unsupported default", in, err)
		}
		if got != RequestScanUnsupported {
			t.Errorf("NormalizeRequestScanState(%q) = %q, want %q: no verdict is not clean", in, got, RequestScanUnsupported)
		}
		if got.SaysClean() {
			t.Errorf("the default scan state %q must not say clean", got)
		}
	}
}

// The gate the contract asks for: unknown is distinct from clean, and only
// clean and explicitly-unsupported downloads pass the scan check.
func TestRequestScanStateGatesDownloadDistinguishUnknownFromClean(t *testing.T) {
	cases := map[RequestScanState]struct {
		saysClean, isUnknown, blocks bool
	}{
		RequestScanClean:       {saysClean: true, isUnknown: false, blocks: false},
		RequestScanUnsupported: {saysClean: false, isUnknown: true, blocks: false},
		RequestScanPending:     {saysClean: false, isUnknown: false, blocks: true},
		RequestScanInfected:    {saysClean: false, isUnknown: false, blocks: true},
	}
	for state, want := range cases {
		if got := state.SaysClean(); got != want.saysClean {
			t.Errorf("%q.SaysClean() = %v, want %v", state, got, want.saysClean)
		}
		if got := state.IsUnknown(); got != want.isUnknown {
			t.Errorf("%q.IsUnknown() = %v, want %v", state, got, want.isUnknown)
		}
		if got := state.BlocksDownload(); got != want.blocks {
			t.Errorf("%q.BlocksDownload() = %v, want %v", state, got, want.blocks)
		}
	}
	// The distinction this type exists for, asserted directly.
	if RequestScanUnsupported.SaysClean() || RequestScanClean.IsUnknown() {
		t.Error("unsupported and clean must be different answers: one is the absence of a verdict, the other is the verdict")
	}
	// An unrecognized state must fail closed rather than serve bytes.
	if !RequestScanState("something-new").BlocksDownload() {
		t.Error("an unknown scan state must block a download")
	}
}

// The bounds are the approved contract's numbers, from the mockups that were
// signed off: 25 MB a file, 5 files, 60 MB a request, 90 days after close.
func TestRequestAttachmentBoundsAreTheApprovedContract(t *testing.T) {
	cases := []struct {
		name string
		got  int64
		want int64
	}{
		{"MaxRequestAttachmentBytes", MaxRequestAttachmentBytes, 25 << 20},
		{"MaxRequestAttachments", MaxRequestAttachments, 5},
		{"MaxRequestAttachmentsTotalBytes", MaxRequestAttachmentsTotalBytes, 60 << 20},
		{"RequestAttachmentRetentionDays", RequestAttachmentRetentionDays, 90},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %d, want the approved %d", c.name, c.got, c.want)
		}
	}
	// The total must bind before the count could ever stand in for it: five
	// files at the per-file limit are 125 MB, above the 60 MB total. If these
	// ever meet, one check silently stops doing anything.
	if MaxRequestAttachmentsTotalBytes >= MaxRequestAttachments*MaxRequestAttachmentBytes {
		t.Errorf("the total bound (%d) must be below count x per-file (%d), or the total check never binds",
			MaxRequestAttachmentsTotalBytes, MaxRequestAttachments*MaxRequestAttachmentBytes)
	}
	if got := RequestAttachmentRetention(); got != 90*24*time.Hour {
		t.Errorf("RequestAttachmentRetention() = %v, want 90 days", got)
	}
	// Serving posture: never inline.
	if RequestAttachmentDisposition != "attachment" {
		t.Errorf("RequestAttachmentDisposition = %q, want the only safe value \"attachment\"", RequestAttachmentDisposition)
	}
}

// Per-file and set bounds both refuse over-limit input, at the boundary.
func TestRequestAttachmentBounds(t *testing.T) {
	a := validAttachment()
	a.SizeBytes = MaxRequestAttachmentBytes
	if err := a.Validate(); err != nil {
		t.Errorf("a file exactly at the per-file limit must be accepted: %v", err)
	}
	a.SizeBytes = MaxRequestAttachmentBytes + 1
	if err := a.Validate(); err == nil || !strings.Contains(err.Error(), "per-file limit") {
		t.Errorf("a file over the per-file limit must be refused naming the limit, got %v", err)
	}
	a.SizeBytes = 0
	if err := a.Validate(); err == nil {
		t.Error("an empty file must be refused: nothing to attach")
	}

	at := func(n int, size int64) []RequestAttachment {
		out := make([]RequestAttachment, n)
		for i := range out {
			out[i] = RequestAttachment{SizeBytes: size}
		}
		return out
	}
	if err := ValidateRequestAttachmentSet(at(MaxRequestAttachments, 1)); err != nil {
		t.Errorf("exactly %d attachments must be accepted: %v", MaxRequestAttachments, err)
	}
	if err := ValidateRequestAttachmentSet(at(MaxRequestAttachments+1, 1)); err == nil || !strings.Contains(err.Error(), "at most 5") {
		t.Errorf("over-count must be refused naming the limit, got %v", err)
	}
	// Under the count limit but over the total: the check that catches five
	// large files.
	fat := at(MaxRequestAttachments, MaxRequestAttachmentBytes)
	if err := ValidateRequestAttachmentSet(fat); err == nil || !strings.Contains(err.Error(), "total") {
		t.Errorf("five files at the per-file limit must be refused by the total, got %v", err)
	}
	if got := RequestAttachmentsTotalBytes(fat); got != 5*(25<<20) {
		t.Errorf("RequestAttachmentsTotalBytes = %d, want %d", got, 5*(25<<20))
	}
	// Exactly the total limit passes.
	exact := at(3, 20<<20)
	if err := ValidateRequestAttachmentSet(exact); err != nil {
		t.Errorf("a set exactly at the total limit must be accepted: %v", err)
	}
}

// D3 exactly: nothing expires on an active request, 90 days after close
// otherwise, and a keep pins it forever.
func TestRequestAttachmentRetention(t *testing.T) {
	closed := requestTime(2026, time.November, 1)

	if got := RequestAttachmentExpiresAt(nil, false); got != nil {
		t.Errorf("an active request's attachments must not expire, got %v", got)
	}
	got := RequestAttachmentExpiresAt(&closed, false)
	if got == nil {
		t.Fatal("a closed request's unkept attachments must expire")
	}
	if want := requestTime(2027, time.January, 30); !got.Equal(want) {
		t.Errorf("expiry = %v, want 90 days after close (%v)", got, want)
	}
	if got := RequestAttachmentExpiresAt(&closed, true); got != nil {
		t.Errorf("a kept attachment must never expire, got %v", got)
	}

	// The row-level question the purge job asks.
	a := validAttachment()
	a.ExpiresAt = RequestAttachmentExpiresAt(&closed, false)
	if a.Expired(got.Add(-time.Second)) {
		t.Error("an attachment must not be purged before its expiry")
	}
	if !a.Expired(*got) {
		t.Error("an attachment must be purgeable at its expiry, not a moment later")
	}
	if !a.Expired(got.Add(time.Second)) {
		t.Error("an attachment must stay purgeable once its expiry passes")
	}
	a.Keep = true
	a.KeptAt = &closed
	if a.Expired(got.Add(365 * 24 * time.Hour)) {
		t.Error("a kept attachment must never be purged, however long it lives")
	}
	a.Keep = false
	if a.Expired(requestTime(2026, time.November, 2)) {
		t.Error("an attachment on an active request (no expiry) must not be purged")
	}
}

// The stored type is the sniffed type: every type the allowlist accepts is one
// Go's sniffer actually returns for bytes that exist. This is what makes "never
// the client's Content-Type" enforceable rather than aspirational — a client
// cannot choose a stored type that the sniffer could not have produced.
func TestRequestAttachmentTypeAllowlistMatchesSniffedType(t *testing.T) {
	samples := map[string][]byte{
		"application/pdf":          []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n"),
		"text/html":                []byte("<!doctype html><html><body>hi</body></html>"),
		"text/xml":                 []byte(`<?xml version="1.0"?><EntityDescriptor></EntityDescriptor>`),
		"text/plain":               []byte("just some notes\n"),
		"image/png":                []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"),
		"image/jpeg":               []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00"),
		"image/gif":                []byte("GIF89a\x01\x00\x01\x00"),
		"image/webp":               []byte("RIFF\x24\x00\x00\x00WEBPVP8 "),
		"image/bmp":                []byte("BM\x36\x00\x00\x00\x00\x00"),
		"application/zip":          []byte("PK\x03\x04\x14\x00\x00\x00"),
		"application/x-gzip":       []byte("\x1f\x8b\x08\x00\x00\x00"),
		"application/octet-stream": []byte("MZ\x90\x00\x03\x00\x00\x00"),
		"application/wasm":         []byte("\x00asm\x01\x00\x00\x00"),
	}
	for wantType, content := range samples {
		sniffed := http.DetectContentType(content)
		base := strings.TrimSpace(strings.SplitN(sniffed, ";", 2)[0])
		if base != wantType {
			t.Errorf("http.DetectContentType returned %q for the %s sample: the allowlist must only name types the sniffer produces", sniffed, wantType)
		}
		if _, ok := RequestAttachmentKindOf(sniffed); !ok {
			t.Errorf("the allowlist must accept %q (a type the sniffer returns)", sniffed)
		}
		if _, _, err := NormalizeRequestAttachmentContentType(sniffed); err != nil {
			t.Errorf("NormalizeRequestAttachmentContentType(%q) = %v, want the sniffed type accepted", sniffed, err)
		}
	}
	// Every allowed type is in the table the allowlist is built from, and the
	// table names nothing outside the samples above (so the test cannot pass
	// while an unverified type creeps in).
	if len(requestAttachmentTypeKinds) != len(samples) {
		var extra []string
		for typ := range requestAttachmentTypeKinds {
			if _, ok := samples[typ]; !ok {
				extra = append(extra, typ)
			}
		}
		t.Errorf("the allowlist has types no sample verified: %v", extra)
	}
}

// A client's content type is refused when a sniffer could not have produced
// it, and the sniffed form is normalized (case, charset parameter).
func TestNormalizeRequestAttachmentContentType(t *testing.T) {
	cases := map[string]struct {
		want     string
		kind     RequestAttachmentKind
		wantFail bool
	}{
		"text/plain; charset=utf-8": {"text/plain", RequestAttachmentText, false},
		"  TEXT/XML  ":              {"text/xml", RequestAttachmentDocument, false},
		"Application/PDF":           {"application/pdf", RequestAttachmentDocument, false},
		"image/png":                 {"image/png", RequestAttachmentImage, false},
		"application/zip":           {"application/zip", RequestAttachmentArchive, false},
		"application/octet-stream":  {"application/octet-stream", RequestAttachmentExecutable, false},
		// Types AMP stores but no sniffer produces are refused: a client
		// declaring one has told us nothing about the bytes.
		"application/json":         {wantFail: true},
		"text/csv":                 {wantFail: true},
		"text/markdown":            {wantFail: true},
		"image/svg+xml":            {wantFail: true},
		"application/x-sh":         {wantFail: true},
		"application/x-msdownload": {wantFail: true},
		"video/mp4":                {wantFail: true},
		"":                         {wantFail: true},
	}
	for in, want := range cases {
		got, kind, err := NormalizeRequestAttachmentContentType(in)
		if want.wantFail {
			if err == nil {
				t.Errorf("NormalizeRequestAttachmentContentType(%q) = %q, want a refusal: no sniffer produces it", in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizeRequestAttachmentContentType(%q) = %v, want %q", in, err, want.want)
			continue
		}
		if got != want.want || kind != want.kind {
			t.Errorf("NormalizeRequestAttachmentContentType(%q) = %q/%q, want %q/%q", in, got, kind, want.want, want.kind)
		}
	}
}

// The restricted-file hold: archives and executables wait for a team lead,
// and a code-like name holds a file whose bytes look like text. A release is
// what lifts it, and a name can never lift one.
func TestRequestAttachmentHold(t *testing.T) {
	cases := []struct {
		name, contentType, fileName string
		released                    bool
		wantHeld                    bool
	}{
		{"xml document", "text/xml", "idp-metadata.xml", false, false},
		{"pdf", "application/pdf", "contract.pdf", false, false},
		{"image", "image/png", "screenshot-login.png", false, false},
		{"plain text", "text/plain", "notes.txt", false, false},
		{"zip archive", "application/zip", "logs-bundle.zip", false, true},
		{"windows binary", "application/octet-stream", "berlin-rollout.exe", false, true},
		{"unnamed tar", "application/octet-stream", "backup.tar", false, true},
		{"script in text clothing", "text/plain", "setup.sh", false, true},
		{"powershell script", "text/plain", "deploy.ps1", false, true},
		{"released archive", "application/zip", "logs-bundle.zip", true, false},
		{"released binary", "application/octet-stream", "berlin-rollout.exe", true, false},
		{"type we cannot classify", "application/x-unknown", "thing", false, true},
	}
	for _, c := range cases {
		a := validAttachment()
		a.ContentType, a.FileName = c.contentType, c.fileName
		if c.released {
			at := requestTime(2026, time.November, 10)
			a.ReleasedAt, a.ReleasedBy = &at, ptrInt(9)
		}
		if got := a.Held(); got != c.wantHeld {
			t.Errorf("%s (%q, %q): Held() = %v, want %v", c.name, c.contentType, c.fileName, got, c.wantHeld)
		}
	}
	// A release cannot lift the sniffed-kind hold for a file whose name is
	// ordinary but whose bytes are a zip; and a released ordinary file is
	// still ordinary.
	if RequestAttachmentRestricted(RequestAttachmentArchive) != true ||
		RequestAttachmentRestricted(RequestAttachmentExecutable) != true {
		t.Error("archives and executables must be restricted kinds")
	}
	if RequestAttachmentRestricted(RequestAttachmentDocument) || RequestAttachmentRestricted(RequestAttachmentImage) || RequestAttachmentRestricted(RequestAttachmentText) {
		t.Error("documents, images and text must not be restricted kinds")
	}
	// A name can escalate a hold but never lift one.
	if !RequestAttachmentHeldByName("setup.SH") {
		t.Error("a script name must be held whatever its case")
	}
	if RequestAttachmentHeldByName("idp-metadata.xml") {
		t.Error("a document name must not be held by name")
	}
	if !RequestAttachmentRestricted(RequestAttachmentArchive) {
		t.Error("a zip named like a document is still an archive")
	}
}

// The download decision is one function: a scan verdict, then the hold, then
// expiry. Each refusal is distinguishable, and a clean unheld file is served.
func TestRequestAttachmentDownloadRefusal(t *testing.T) {
	closed := requestTime(2026, time.November, 1)
	now := requestTime(2026, time.November, 10)
	past := requestTime(2027, time.February, 1)

	cases := []struct {
		name string
		mut  func(*RequestAttachment)
		now  time.Time
		want RequestAttachmentRefusal
	}{
		{"clean document", func(a *RequestAttachment) {}, now, RequestAttachmentServed},
		{"unscanned is unknown, not blocked", func(a *RequestAttachment) { a.ScanState = RequestScanUnsupported }, now, RequestAttachmentServed},
		{"still pending", func(a *RequestAttachment) { a.ScanState = RequestScanPending }, now, RequestAttachmentRefusedPending},
		{"infected", func(a *RequestAttachment) { a.ScanState = RequestScanInfected }, now, RequestAttachmentRefusedInfected},
		{"held executable", func(a *RequestAttachment) { a.ContentType = "application/octet-stream"; a.FileName = "rollout.exe" }, now, RequestAttachmentRefusedHeld},
		{"held script", func(a *RequestAttachment) { a.FileName = "setup.sh" }, now, RequestAttachmentRefusedHeld},
		{"released executable", func(a *RequestAttachment) {
			a.ContentType, a.FileName = "application/octet-stream", "rollout.exe"
			at := now
			a.ReleasedAt, a.ReleasedBy = &at, ptrInt(9)
		}, now, RequestAttachmentServed},
		{"purged after retention", func(a *RequestAttachment) { a.ExpiresAt = RequestAttachmentExpiresAt(&closed, false) }, past, RequestAttachmentRefusedPurged},
		{"inside retention", func(a *RequestAttachment) { a.ExpiresAt = RequestAttachmentExpiresAt(&closed, false) }, now, RequestAttachmentServed},
		{"kept never purged", func(a *RequestAttachment) {
			a.ExpiresAt = RequestAttachmentExpiresAt(&closed, true)
			a.Keep, a.KeptAt = true, &closed
		}, past, RequestAttachmentServed},
		{"held beats purged as the actionable answer", func(a *RequestAttachment) {
			a.ContentType, a.FileName = "application/zip", "logs.zip"
			a.ExpiresAt = RequestAttachmentExpiresAt(&closed, false)
		}, past, RequestAttachmentRefusedHeld},
	}
	for _, c := range cases {
		a := validAttachment()
		c.mut(a)
		if got := RequestAttachmentDownloadRefusal(a, c.now); got != c.want {
			t.Errorf("%s: DownloadRefusal = %q, want %q", c.name, got, c.want)
		}
	}
}

// The record's shape: request scope, display name (never a path), a stored
// type, a size within bounds, a digest, a known scan state, and coherent
// keep/release columns. In-place normalization matches the record it returns.
func TestRequestAttachmentValidate(t *testing.T) {
	if err := validAttachment().Validate(); err != nil {
		t.Fatalf("a valid attachment must pass: %v", err)
	}

	at := requestTime(2026, time.November, 10)
	cases := map[string]struct {
		mut  func(*RequestAttachment)
		want string
	}{
		"no request":            {func(a *RequestAttachment) { a.RequestID = 0 }, "exactly one request"},
		"empty name":            {func(a *RequestAttachment) { a.FileName = "  " }, "file_name is required"},
		"path in name":          {func(a *RequestAttachment) { a.FileName = "../../etc/passwd" }, "not a path"},
		"windows path in name":  {func(a *RequestAttachment) { a.FileName = `C:\tmp\x.txt` }, "not a path"},
		"dot dot name":          {func(a *RequestAttachment) { a.FileName = ".." }, "not a path"},
		"control char in name":  {func(a *RequestAttachment) { a.FileName = "a\x00b.txt" }, "control characters"},
		"long name":             {func(a *RequestAttachment) { a.FileName = strings.Repeat("x", MaxRequestAttachmentNameLen+1) }, "file_name is over"},
		"client type":           {func(a *RequestAttachment) { a.ContentType = "application/json" }, "not a type AMP stores"},
		"no type":               {func(a *RequestAttachment) { a.ContentType = "" }, "not a type AMP stores"},
		"zero size":             {func(a *RequestAttachment) { a.SizeBytes = 0 }, "size_bytes must be positive"},
		"no digest":             {func(a *RequestAttachment) { a.SHA256 = "" }, "sha256 is required"},
		"short digest":          {func(a *RequestAttachment) { a.SHA256 = "abc" }, "64 hex characters"},
		"non-hex digest":        {func(a *RequestAttachment) { a.SHA256 = strings.Repeat("z", 64) }, "64 hex characters"},
		"unknown scan state":    {func(a *RequestAttachment) { a.ScanState = "scanned" }, "scan_state must be one of"},
		"keep without moment":   {func(a *RequestAttachment) { a.Keep = true }, "records when it was pinned"},
		"keeper without moment": {func(a *RequestAttachment) { a.KeptBy = ptrInt(9) }, "kept_at"},
		"releaser without moment": {func(a *RequestAttachment) {
			a.ContentType, a.FileName = "application/zip", "logs.zip"
			a.ReleasedBy = ptrInt(9)
		}, "released_at"},
		"released but never held": {func(a *RequestAttachment) {
			a.ReleasedAt, a.ReleasedBy = &at, ptrInt(9)
		}, "never restricted"},
	}
	for name, c := range cases {
		a := validAttachment()
		c.mut(a)
		err := a.Validate()
		if err == nil {
			t.Errorf("%s must be refused", name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s error = %q, want it to contain %q", name, err, c.want)
		}
	}

	a := validAttachment()
	a.FileName, a.ContentType = "  IdP-Metadata.XML  ", "TEXT/XML; charset=utf-8"
	a.SHA256, a.ScanState = strings.Repeat("AB", 32), "  CLEAN "
	if err := a.Validate(); err != nil {
		t.Fatalf("a normalizable attachment must pass: %v", err)
	}
	if a.FileName != "IdP-Metadata.XML" {
		t.Errorf("file_name = %q, want it trimmed", a.FileName)
	}
	if a.ContentType != "text/xml" {
		t.Errorf("content_type = %q, want the sniffed type normalized to text/xml", a.ContentType)
	}
	if a.SHA256 != strings.Repeat("ab", 32) {
		t.Errorf("sha256 = %q, want it lower-cased", a.SHA256)
	}
	if a.ScanState != RequestScanClean {
		t.Errorf("scan_state = %q, want clean", a.ScanState)
	}
	// A valid record must always be storable: a required scan state, a digest
	// that is a digest, and a size the schema's BIGINT and the bound both hold.
	if a.ScanState == "" {
		t.Error("Validate must leave a known scan_state, never empty")
	}
}

// An empty scan state normalizes to unknown through Validate too, so a write
// path that never sets one cannot claim clean.
func TestRequestAttachmentValidateNeverInventsClean(t *testing.T) {
	a := validAttachment()
	a.ScanState = ""
	if err := a.Validate(); err != nil {
		t.Fatalf("an unset scan state must default rather than fail: %v", err)
	}
	if a.ScanState.SaysClean() {
		t.Fatal("an attachment with no scan verdict must never validate as clean")
	}
	if a.ScanState != RequestScanUnsupported {
		t.Errorf("scan_state = %q, want the unsupported default", a.ScanState)
	}
}

// The domain file stays metadata-only: no storage, no network, no sniffing, no
// generator. This is the ticket's "no storage/upload/download code" boundary
// made a test rather than a promise.
func TestRequestAttachmentDomainHasNoStorageOrNetworkImports(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "request_attachment.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse request_attachment.go: %v", err)
	}
	for _, spec := range f.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatalf("import path %s: %v", spec.Path.Value, err)
		}
		switch path {
		case "errors", "fmt", "path", "slices", "strings", "time", "unicode", "unicode/utf8":
		default:
			t.Errorf("request_attachment.go imports %q: the domain file must stay free of storage, network, sniffing and generator code", path)
		}
	}
}

// The record carries the metadata the team panel and the three gates need: the
// sniffed type, size, digest, scan state, retention columns and the release
// columns — and never the bytes by default.
func TestRequestAttachmentRecordShape(t *testing.T) {
	for _, field := range []string{
		"request_id", "file_name", "content_type", "size_bytes", "sha256",
		"scan_state", "uploaded_by", "created_at", "keep", "kept_by", "kept_at",
		"expires_at", "released_at", "released_by",
	} {
		if !hasField(&RequestAttachment{}, field) {
			t.Errorf("RequestAttachment has no JSON field %q", field)
		}
	}
	raw, err := json.Marshal(validAttachment())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := fields["content"]; ok {
		t.Error("content must be excluded from JSON: bytes never travel with the metadata")
	}
	if _, ok := fields["scan_state"]; !ok {
		t.Error("scan_state must travel with the metadata: it is what gates a download")
	}
}
