package core

// VerifiedRecord and VerifiedEntry can only be obtained from Verified, after
// the signature (G1) has been checked, so a consumer that takes one cannot
// forget to verify. They wrap the immutable parsed value.
//
// The zero value holds nothing: every accessor returns the zero value of its
// type (empty id and author, nil bytes, empty fields), which no check
// accepts as a real id or node id. Wire returns nil.

// VerifiedRecord is a record whose signature by its author has been checked.
type VerifiedRecord struct{ r *Record }

// Verified checks the signature (G1) and returns the record as verified.
func (r *Record) Verified() (*VerifiedRecord, error) {
	if err := r.VerifySignature(); err != nil {
		return nil, err
	}
	return &VerifiedRecord{r}, nil
}

func (v *VerifiedRecord) ok() bool { return v != nil && v.r != nil }

func (v *VerifiedRecord) Fields() RecordFields {
	if !v.ok() {
		return RecordFields{}
	}
	return v.r.Fields()
}

func (v *VerifiedRecord) ID() string {
	if !v.ok() {
		return ""
	}
	return v.r.ID()
}

func (v *VerifiedRecord) Author() string {
	if !v.ok() {
		return ""
	}
	return v.r.Author()
}

func (v *VerifiedRecord) BodyHash() string {
	if !v.ok() {
		return ""
	}
	return v.r.BodyHash()
}

func (v *VerifiedRecord) Size() int64 {
	if !v.ok() {
		return 0
	}
	return v.r.Size()
}

func (v *VerifiedRecord) Header() []byte {
	if !v.ok() {
		return nil
	}
	return v.r.Header()
}

func (v *VerifiedRecord) Sig() []byte {
	if !v.ok() {
		return nil
	}
	return v.r.Sig()
}

// Body returns the body bytes, or nil if no text is held.
func (v *VerifiedRecord) Body() []byte {
	if !v.ok() {
		return nil
	}
	return v.r.Body()
}

// WithoutBody returns the same verified record with no text; the header and
// signature, which were verified, are unchanged.
func (v *VerifiedRecord) WithoutBody() *VerifiedRecord {
	if !v.ok() {
		return &VerifiedRecord{}
	}
	return &VerifiedRecord{v.r.WithoutBody()}
}

// Wire returns the wire object {"h","s"[,"b"]}, or nil for the zero value.
func (v *VerifiedRecord) Wire() []byte {
	if !v.ok() {
		return nil
	}
	return v.r.Wire()
}

// VerifiedEntry is a log entry whose signature by its author has been
// checked.
type VerifiedEntry struct{ e *Entry }

// Verified checks the signature (G1) and returns the entry as verified.
func (e *Entry) Verified() (*VerifiedEntry, error) {
	if err := e.VerifySignature(); err != nil {
		return nil, err
	}
	return &VerifiedEntry{e}, nil
}

func (v *VerifiedEntry) ok() bool { return v != nil && v.e != nil }

func (v *VerifiedEntry) Fields() EntryFields {
	if !v.ok() {
		return EntryFields{}
	}
	return v.e.Fields()
}

func (v *VerifiedEntry) ID() string {
	if !v.ok() {
		return ""
	}
	return v.e.ID()
}

func (v *VerifiedEntry) Author() string {
	if !v.ok() {
		return ""
	}
	return v.e.Author()
}

func (v *VerifiedEntry) Header() []byte {
	if !v.ok() {
		return nil
	}
	return v.e.Header()
}

func (v *VerifiedEntry) Sig() []byte {
	if !v.ok() {
		return nil
	}
	return v.e.Sig()
}

// Wire returns the wire object {"h","s"}, or nil for the zero value.
func (v *VerifiedEntry) Wire() []byte {
	if !v.ok() {
		return nil
	}
	return v.e.Wire()
}
