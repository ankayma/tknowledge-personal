package tka

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Evidence a person can go and look at (mvp/tk-document-quan-he-T-D-02-10-26.md).
// tk is a store of knowledge, not of files: a document is the text someone read
// out of the evidence, how they looked at it, and where the original lives —
// Drive, S3, the owner's laptop, a public page. The original stays where it is.
// The text is what the ledger reads, so it is the one thing a document cannot
// lack. A document has an owner: the person who added it, from their grant.

// MaxDocument is the most text one document carries. Evidence is a page that
// carries the claim, not the archive it came from.
const MaxDocument = 10 << 20 // 10 MB

// Who wrote the text: the person, the AI reading for them, or an extractor.
// "agent" is any AI; which one is the row's producer, from the grant.
// "claude" stays accepted for clients that learned it first.
var textProducers = []string{"owner", "agent", "claude", "extract"}

// How the person who added the document looked at the evidence. The order is
// the strength a claim reads from (B8, owner approved 2/10): someone who opened the
// system of record saw more than someone who checked a hash of a copy they were
// handed, who saw more than someone who only read another document's text.
var methods = []string{"opened_source", "rehashed", "read_text"}

// Where an original may live. A scheme outside this list is a typo more often
// than a new kind of store; adding one is a line here, not a schema change.
var sourceSchemes = map[string]bool{"https": true, "http": true, "drive": true, "s3": true, "file": true}

var hexDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

type Document struct {
	ID            string     `json:"id"`
	Text          *string    `json:"text"`
	TextProducer  *string    `json:"text_producer,omitempty"`
	Method        *string    `json:"method,omitempty"`
	Locator       *string    `json:"locator,omitempty"`
	SourceURI     *string    `json:"source_uri,omitempty"`
	SHA256        string     `json:"sha256,omitempty"`
	Size          *int       `json:"size,omitempty"`
	ContentSHA256 *string    `json:"content_sha256,omitempty"`
	Custodian     *string    `json:"custodian,omitempty"`
	CapturedAt    *time.Time `json:"captured_at,omitempty"`
	MediaType     string     `json:"media_type,omitempty"`
	Filename      string     `json:"filename,omitempty"`
	Signer        string     `json:"signer"`
	Producer      string     `json:"producer"`
	GrantID       string     `json:"grant_id"`
	AddedAt       time.Time  `json:"added_at"`
	// Bound says whether anything ties this text to an original: a source_uri, or
	// the sha256 of the original. A document that is only text reads bound: false
	// on every read — not a warning that scrolls away once (P.3).
	Bound bool `json:"bound"`
	// Legacy marks a document added before 2/10/2026, when the store kept the
	// file itself. Its sha256 is of the stored file, not of an original, so it does
	// not bind; one that was not text has no text either.
	Legacy bool `json:"legacy,omitempty"`
	// EvidenceFor are the claims this document stands behind right now (B3: not
	// `supports`, which already means where an entry is cited).
	EvidenceFor []string `json:"evidence_for"`
	// Flags opened by this add — the same page read twice and found changed, or
	// read differently. Only on the answer to document_add.
	Flags []Flag `json:"flags,omitempty"`
	// Thread is set only on the answer to document_add when the caller named a
	// thread: which one the document was linked to. It is not part of the record.
	Thread *ThreadLinked `json:"thread,omitempty"`
}

type DocumentInput struct {
	Text          string `json:"text"`
	TextProducer  string `json:"text_producer"`
	Method        string `json:"method"`
	Locator       string `json:"locator"`
	SourceURI     string `json:"source_uri"`
	SHA256        string `json:"sha256"`
	Size          *int   `json:"size"`
	ContentSHA256 string `json:"content_sha256"`
	Custodian     string `json:"custodian"`
	CapturedAt    string `json:"captured_at"`
	MediaType     string `json:"media_type"`
	Filename      string `json:"filename"`
	// Thread links the document to a thread (TH-… or a keyword) in the same
	// transaction — also when the same original was already stored.
	Thread string `json:"thread"`
	// Content is the old shape: the file itself, base64. It is refused, so a
	// caller still sending it is told what to send instead.
	Content string `json:"content"`
}

type checkedDocument struct {
	text, producer, method                                      string
	locator, sourceURI, sha, contentSHA, custodian, media, name *string
	size                                                        *int
	capturedAt                                                  *time.Time
}

func optional(s string) *string {
	if v := strings.TrimSpace(s); v != "" {
		return &v
	}
	return nil
}

func oneOf(raw string, allowed []string) (string, bool) {
	v := strings.ToLower(strings.TrimSpace(raw))
	for _, a := range allowed {
		if v == a {
			return v, true
		}
	}
	return v, false
}

func checkDigest(label, raw string) (*string, error) {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" {
		return nil, nil
	}
	if !hexDigest.MatchString(v) {
		return nil, fail("INVALID_SHA256", "%s must be 64 hexadecimal characters", label)
	}
	return &v, nil
}

func checkSourceURI(raw string) (*string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return nil, nil
	}
	u, err := url.Parse(v)
	ok := err == nil && sourceSchemes[strings.ToLower(u.Scheme)]
	if ok {
		switch strings.ToLower(u.Scheme) {
		case "http", "https", "s3":
			ok = u.Host != ""
		default: // drive://<id>, file:///path
			ok = u.Host != "" || strings.Trim(u.Path, "/") != "" || u.Opaque != ""
		}
	}
	if !ok {
		return nil, fail("INVALID_SOURCE_URI", "%q does not say where an original lives", v)
	}
	return &v, nil
}

func parseInstant(label, raw string) (*time.Time, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return nil, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, v); err == nil {
			return &t, nil
		}
	}
	return nil, fail("INVALID_DATE", "%s must be YYYY-MM-DD or an RFC 3339 time", label)
}

func checkDocumentInput(in DocumentInput) (checkedDocument, error) {
	var out checkedDocument
	if strings.TrimSpace(in.Content) != "" {
		return out, fail("FILE_NOT_KEPT", "tk keeps no files: a document is the text read from the evidence and a pointer to where the original lives")
	}
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return out, fail("EMPTY_TEXT", "a document the ledger cannot read is not evidence yet")
	}
	if len(text) > MaxDocument {
		return out, fail("DOCUMENT_TOO_LARGE", "a document's text is at most %d MB, this one is %.1f MB",
			MaxDocument>>20, float64(len(text))/(1<<20))
	}
	producer, ok := oneOf(in.TextProducer, textProducers)
	if !ok {
		return out, fail("INVALID_TEXT_PRODUCER", "text_producer must be one of %s", strings.Join(textProducers, ", "))
	}
	method, ok := oneOf(in.Method, methods)
	if !ok {
		return out, fail("INVALID_METHOD", "method must be one of %s", strings.Join(methods, ", "))
	}
	var err error
	if out.sourceURI, err = checkSourceURI(in.SourceURI); err != nil {
		return out, err
	}
	if out.sha, err = checkDigest("sha256", in.SHA256); err != nil {
		return out, err
	}
	if out.contentSHA, err = checkDigest("content_sha256", in.ContentSHA256); err != nil {
		return out, err
	}
	if out.capturedAt, err = parseInstant("captured_at", in.CapturedAt); err != nil {
		return out, err
	}
	if in.Size != nil && *in.Size <= 0 {
		return out, fail("INVALID_SIZE", "size is the original's size in bytes, more than 0")
	}
	if m := optional(in.MediaType); m != nil {
		lower := strings.ToLower(*m)
		if typ, sub, found := strings.Cut(lower, "/"); !found || typ == "" || sub == "" {
			return out, fail("INVALID_MEDIA_TYPE", "%q is not a media type (type/subtype)", *m)
		}
		out.media = &lower
	}
	out.text, out.producer, out.method = text, producer, method
	out.locator, out.custodian, out.name, out.size = optional(in.Locator), optional(in.Custodian), optional(in.Filename), in.Size
	return out, nil
}

// AddDocument stores one piece of evidence and returns its id. The same original
// (by sha256) added twice is one document: the second call returns the first,
// so re-running an import never fills the store with copies.
func (r *Repo) AddDocument(ctx context.Context, a Actor, in DocumentInput) (Document, error) {
	if err := r.checkSigner(a); err != nil {
		return Document{}, err
	}
	c, err := checkDocumentInput(in)
	if err != nil {
		return Document{}, err
	}
	var doc Document
	err = r.withTenant(ctx, a.Tenant(), func(q querier) error {
		if err := r.writable(ctx, q, a); err != nil {
			return err
		}
		if err := checkThreadRef(ctx, q, a.Tenant(), in.Thread); err != nil {
			return err
		}
		if c.sha != nil {
			if d, err := bySHA(ctx, q, a.Tenant(), *c.sha); err == nil {
				doc = d // the same original, already here
				doc.Thread, err = r.linkWritten(ctx, q, a, in.Thread, d.ID, "document_add")
				return err
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		num, err := nextID(ctx, q, a.Tenant(), "D")
		if err != nil {
			return err
		}
		var id string
		if err := q.QueryRow(ctx, `
			INSERT INTO document (tenant_id, num, text, text_producer, method, locator, source_uri, sha256, size,
				content_sha256, custodian, captured_at, media_type, filename, signer, producer, grant_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17) RETURNING id`,
			a.Tenant(), num, c.text, c.producer, c.method, c.locator, c.sourceURI, c.sha, c.size,
			c.contentSHA, c.custodian, c.capturedAt, c.media, c.name, a.UserID, a.ActorID, a.GrantID).Scan(&id); err != nil {
			return err
		}
		flags, err := pageFlags(ctx, q, a, id, c)
		if err != nil {
			return err
		}
		if doc, err = readDocument(ctx, q, a.Tenant(), id); err != nil {
			return err
		}
		doc.Flags = flags
		doc.Thread, err = r.linkWritten(ctx, q, a, in.Thread, id, "document_add")
		return err
	})
	return doc, err
}

// pageFlags compares a new document with the others that read the same
// source_uri. A live page is a live source (T-ledger §3.1): a different
// content_sha256 means the page changed between two readings; the same hash
// with different text means two people read the same page differently. The
// two are different findings and are flagged apart.
func pageFlags(ctx context.Context, q querier, a Actor, id string, c checkedDocument) ([]Flag, error) {
	out := []Flag{}
	if c.sourceURI == nil || c.contentSHA == nil {
		return out, nil
	}
	rows, err := q.Query(ctx, `
		SELECT id, content_sha256, text FROM document
		WHERE tenant_id = $1 AND source_uri = $2 AND content_sha256 IS NOT NULL AND id <> $3
		ORDER BY num`, a.Tenant(), *c.sourceURI, id)
	if err != nil {
		return nil, err
	}
	type other struct{ id, hash, text string }
	others := []other{}
	for rows.Next() {
		var o other
		var text *string
		if err := rows.Scan(&o.id, &o.hash, &text); err != nil {
			rows.Close()
			return nil, err
		}
		if text != nil {
			o.text = *text
		}
		others = append(others, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var changed, differs []string
	for _, o := range others {
		switch {
		case o.hash != *c.contentSHA:
			changed = append(changed, o.id)
		case strings.TrimSpace(o.text) != c.text:
			differs = append(differs, o.id)
		}
	}
	if len(changed) > 0 {
		f, err := openFlag(ctx, q, a, "page_changed", id, fmt.Sprintf(
			"%s reads %s with a different content_sha256 from %s: the page changed between the readings",
			id, *c.sourceURI, strings.Join(changed, ", ")))
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	if len(differs) > 0 {
		f, err := openFlag(ctx, q, a, "read_differently", id, fmt.Sprintf(
			"%s and %s read the same page of %s (same content_sha256) into different text",
			id, strings.Join(differs, ", "), *c.sourceURI))
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// Documents lists the evidence this store holds, newest first, text included —
// what the evidence says is the point of listing it.
func (r *Repo) Documents(ctx context.Context, tenant string) ([]Document, error) {
	out := []Document{}
	err := r.withTenant(ctx, tenant, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT `+documentColumns+` FROM document WHERE tenant_id = $1 ORDER BY num DESC`,
			TenantOr(tenant))
		if err != nil {
			return err
		}
		for rows.Next() {
			d, err := scanDocument(rows)
			if err != nil {
				rows.Close()
				return err
			}
			out = append(out, d)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range out {
			if out[i].EvidenceFor, err = evidenceFor(ctx, q, tenant, out[i].ID); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// LookupDocuments finds documents whose text carries what the person typed:
// the words verbatim first, then text that comes close.
func (r *Repo) LookupDocuments(ctx context.Context, tenant, raw string) ([]Document, error) {
	q0 := strings.ToLower(strings.Join(strings.Fields(raw), " "))
	out := []Document{}
	if q0 == "" {
		return out, nil
	}
	err := r.withTenant(ctx, tenant, func(q querier) error {
		rows, err := q.Query(ctx, `
			SELECT `+documentColumns+` FROM document
			WHERE tenant_id = $1 AND text IS NOT NULL
			  AND (strpos(lower(text), $2) > 0 OR word_similarity($2, lower(text)) >= $3)
			ORDER BY (strpos(lower(text), $2) > 0) DESC, word_similarity($2, lower(text)) DESC, num DESC
			LIMIT 10`, TenantOr(tenant), q0, lookupMin)
		if err != nil {
			return err
		}
		for rows.Next() {
			d, err := scanDocument(rows)
			if err != nil {
				rows.Close()
				return err
			}
			out = append(out, d)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range out {
			if out[i].EvidenceFor, err = evidenceFor(ctx, q, tenant, out[i].ID); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// ReadDocument returns one document's record, and for a document added before
// 2/10/2026 the file the store kept then. A document added since has no file:
// its text is the evidence the store holds.
func (r *Repo) ReadDocument(ctx context.Context, tenant, id string) (Document, []byte, error) {
	var doc Document
	var body []byte
	err := r.withTenant(ctx, tenant, func(q querier) error {
		d, err := readDocument(ctx, q, tenant, id)
		if err != nil {
			return err
		}
		if d.EvidenceFor, err = evidenceFor(ctx, q, tenant, d.ID); err != nil {
			return err
		}
		doc = d
		return q.QueryRow(ctx, `SELECT bytes FROM document WHERE tenant_id = $1 AND id = $2`,
			TenantOr(tenant), doc.ID).Scan(&body)
	})
	return doc, body, err
}

const documentColumns = `id, text, text_producer, method, locator, source_uri, sha256, size, content_sha256,
	custodian, captured_at, media_type, filename, signer, producer, grant_id, added_at, bytes IS NOT NULL`

func readDocument(ctx context.Context, q querier, tenant, id string) (Document, error) {
	ref := CanonID(id)
	d, err := scanDocument(q.QueryRow(ctx, `SELECT `+documentColumns+` FROM document WHERE tenant_id = $1 AND id = $2`,
		TenantOr(tenant), ref))
	if errors.Is(err, pgx.ErrNoRows) {
		return Document{}, fail("DOCUMENT_UNKNOWN", "%s is not a document in this store", ref)
	}
	return d, err
}

func bySHA(ctx context.Context, q querier, tenant, digest string) (Document, error) {
	d, err := scanDocument(q.QueryRow(ctx, `SELECT `+documentColumns+` FROM document WHERE tenant_id = $1 AND sha256 = $2`,
		TenantOr(tenant), digest))
	if err != nil {
		return d, err
	}
	d.EvidenceFor, err = evidenceFor(ctx, q, tenant, d.ID)
	return d, err
}

type scanner interface{ Scan(dest ...any) error }

func scanDocument(row scanner) (Document, error) {
	var d Document
	var sha, media, name *string
	if err := row.Scan(&d.ID, &d.Text, &d.TextProducer, &d.Method, &d.Locator, &d.SourceURI, &sha, &d.Size,
		&d.ContentSHA256, &d.Custodian, &d.CapturedAt, &media, &name,
		&d.Signer, &d.Producer, &d.GrantID, &d.AddedAt, &d.Legacy); err != nil {
		return Document{}, err
	}
	if sha != nil {
		d.SHA256 = *sha
	}
	if media != nil {
		d.MediaType = *media
	}
	if name != nil {
		d.Filename = *name
	}
	// A legacy document's sha256 is of the file the store kept, which is the
	// transcription itself — it ties the text to nothing outside the store.
	d.Bound = d.SourceURI != nil || (sha != nil && !d.Legacy)
	d.EvidenceFor = []string{}
	return d, nil
}

// checkEvidence is the gate's evidence check for a fact. A fact rests on a
// document the signer added: their own reading of the evidence, in their own
// text (T-ledger §3.1 — the name on the record is the owner of its first D). A
// bare link is no longer taken: it keeps nothing, and when the page goes the
// evidence goes with it (B1, owner approved 2/10).
func (r *Repo) checkEvidence(ctx context.Context, q querier, a Actor, in BasisInput) (doc, entry *string, err error) {
	if strings.TrimSpace(in.SourceURL) != "" {
		return nil, nil, fail("USE_DOCUMENT", "a link on its own keeps nothing: when the page changes or goes, the evidence goes with it")
	}
	if ref := strings.TrimSpace(in.SourceDoc); ref != "" {
		d, e := readDocument(ctx, q, a.Tenant(), ref)
		if e != nil {
			return nil, nil, e
		}
		if d.Signer != a.UserID {
			return nil, nil, fail("DOCUMENT_NOT_YOURS",
				"%s was added by %s: a claim you sign rests first on evidence you looked at yourself", d.ID, d.Signer)
		}
		doc = &d.ID
	}
	// A claim already in the ledger may carry the part of the evidence a document
	// has no way to carry — when the meeting was, that it happened at all. It is
	// cited as evidence, never counted as a premise: premises are claims reasoned
	// from, and this is a source (owner decision, 28/9).
	if ref := strings.TrimSpace(in.SourceEntry); ref != "" {
		t, e := r.ReadTruth(ctx, q, a.Tenant(), strings.ToUpper(ref))
		if e != nil {
			var tkaErr *Error
			if errors.As(e, &tkaErr) && tkaErr.Code == "NOT_FOUND" {
				return nil, nil, failNext("SOURCE_ENTRY_NOT_FOUND",
					"Check the id with entry(...). Only an entry in the truth ledger can be cited this way.",
					"%s is not an entry in this store", ref)
			}
			return nil, nil, e
		}
		if t.Effective != "T" {
			return nil, nil, failNext("SOURCE_ENTRY_NOT_T",
				"Read it with entry(...): re-establish it first, or cite evidence that stands.",
				"%s is %s, not T — a claim cannot draw evidence from an entry that has fallen", t.ID, t.Effective)
		}
		entry = &t.ID
	}
	// An entry on its own is not enough. A document is always required:
	// otherwise a chain of claims could cite each other and never touch the world.
	if doc == nil {
		return nil, nil, fail("SOURCE_NOT_RETRIEVABLE",
			"a fact rests on a document — the evidence read into text, with a pointer to where the original lives")
	}
	return doc, entry, nil
}
