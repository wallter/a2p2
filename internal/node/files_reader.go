package node

// Reader states of files (PROTOCOL 10.2, 10.6): what an entry says of the
// files of the version shown. Nothing here fetches or opens a file.

import "github.com/wallter/a2p2/internal/store"

// EntryFile is one file of a version shown (PROTOCOL 10.6).
type EntryFile struct {
	SHA256 string `json:"sha256"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Type   string `json:"type,omitempty"`
	State  string `json:"state"`
}

// States of a file in an entry.
const (
	FileStateHere        = "here"        // verified bytes held on this machine
	FileStateNotFetched  = "not_fetched" // fetch it to have it
	FileStatePartial     = "partial"     // a download in progress
	FileStateComing      = "coming"      // asked of the author, who uploads when it next pulls
	FileStateUnavailable = "unavailable" // no holder served it in time
)

// Notes of an entry whose file list will not come.
const (
	FilesNoteInvalid     = "invalid file list"
	FilesNoteUnavailable = "file list unavailable"
)

// fileState fills in e's files from ver, the version shown. A read that
// fails leaves the list pending.
func (v *roomView) fileState(e *Entry, ver *rec) {
	if ver == nil || ver.f.Files == "" {
		return
	}
	l, bad, err := v.rd.FileList(v.room, ver.id)
	if err != nil {
		e.FilesPending = true
		return
	}
	if bad {
		switch l.State {
		case store.ListInvalid:
			e.FilesNote = FilesNoteInvalid
			return
		case store.ListUnavailable:
			e.FilesNote = FilesNoteUnavailable
			return
		}
	}
	_, ok, err := v.rd.FileSet(v.room, ver.id)
	if err != nil || !ok {
		e.FilesPending = true
		return
	}
	items, err := v.rd.FileItems(v.room, ver.id)
	if err != nil {
		e.FilesPending = true
		return
	}
	for _, it := range items {
		ef := EntryFile{SHA256: it.SHA256, Name: it.Name, Size: it.Size, Type: it.Type, State: itemState(v.rd, v.room, e.ID, it.SHA256)}
		e.Files = append(e.Files, ef)
	}
}

// itemState is what this machine holds of file sha of message msg.
func itemState(rd *store.Reader, room, msg, sha string) string {
	how, _ := rd.HeldFor(room, sha, msg)
	_, part, _ := rd.FilePart(room, msg, sha)
	w, want, _ := rd.FileWant(room, msg, sha)
	switch {
	case how != "":
		return FileStateHere
	case part:
		return FileStatePartial
	case want && w.Lapsed.IsZero():
		return FileStateComing
	case want:
		return FileStateUnavailable
	}
	return FileStateNotFetched
}
