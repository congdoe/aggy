package borrow

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/safefs"
)

// The media types the artifact gate accepts, by magic bytes.
const (
	PNG  = "image/png"
	JPEG = "image/jpeg"
	MP4  = "video/mp4"
)

// Size caps per artifact file.
const (
	MaxImageBytes = 50 << 20
	MaxVideoBytes = 256 << 20
)

// timeSlack covers file systems that keep times in whole seconds (HFS+)
// or two (FAT): a file made just after the child started may show a time
// just before it.
const timeSlack = 2 * time.Second

// media is one artifact that passed the gate, with its bytes.
type media struct {
	adapter.Artifact
	Type string // PNG, JPEG, or MP4
	Data []byte
}

// Ext is the file extension of the media type.
func (m media) Ext() string {
	switch m.Type {
	case PNG:
		return ".png"
	case JPEG:
		return ".jpg"
	}
	return ".mp4"
}

// readArtifact is the artifact gate. A file a child left in its vendor's
// store leaves the machine's vendor folders only through here, and it
// passes only when all of these hold:
//
//   - it is a regular file with exactly one link, opened through safefs
//     (no symlink, FIFO, or device; never a credential file);
//   - it was made after the child started (started is the wall clock
//     just before the child ran);
//   - it is within the size cap of its kind;
//   - its magic bytes are those of the kind: PNG or JPEG for an image,
//     MP4 for a video.
//
// That closes the hard-link-before-rotation case safefs cannot: a child
// that hard-links a credential into its artifact folder and waits for the
// vendor to replace the credential by rename is left with an old inode
// that has one link, but it was made before the child started and is not
// an image. Any other file is refused with the reason.
func readArtifact(a adapter.Artifact, started time.Time) (media, error) {
	refuse := func(why string) (media, error) { return media{}, fmt.Errorf("%s: refused, %s", a.Path, why) }
	limit := int64(MaxImageBytes)
	if a.Kind == "video" {
		limit = MaxVideoBytes
	}
	f, err := safefs.OpenRegular(a.Path)
	switch {
	case errors.Is(err, safefs.ErrHardLink):
		return refuse("it has more than one hard link")
	case errors.Is(err, safefs.ErrAuthPath):
		return refuse("it is a vendor credential file")
	case errors.Is(err, safefs.ErrNotRegular):
		return refuse("it is not a regular file")
	case err != nil:
		return refuse(err.Error())
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return refuse(err.Error())
	}
	switch {
	case !fi.Mode().IsRegular():
		return refuse("it is not a regular file")
	case safefs.LinkCount(fi) != 1:
		return refuse(fmt.Sprintf("it has %d hard links", safefs.LinkCount(fi)))
	case born(fi).Before(started.Add(-timeSlack)):
		return media{}, fmt.Errorf("%s: refused, %w", a.Path, errOld)
	case fi.Size() > limit:
		return refuse(fmt.Sprintf("it is over the %d MiB cap", limit>>20))
	}
	// The magic bytes first: a file that fails them is never read whole.
	head := make([]byte, sniffLen)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return refuse(err.Error())
	}
	head = head[:n]
	typ := sniff(head)
	switch {
	case a.Kind == "video" && typ != MP4:
		return refuse("it is not an MP4 video")
	case a.Kind != "video" && typ != PNG && typ != JPEG:
		return refuse("it is not a PNG or JPEG image")
	}
	rest, err := readRest(io.LimitReader(f, limit+1-int64(n)))
	if err != nil {
		return refuse(err.Error())
	}
	data := append(head, rest...)
	if int64(len(data)) > limit {
		return refuse(fmt.Sprintf("it is over the %d MiB cap", limit>>20))
	}
	return media{Artifact: a, Type: typ, Data: data}, nil
}

// errOld is the refusal of a file made before the child started.
var errOld = errors.New("it was made before the child started")

// born is when the file was made: its birth time where the system reports
// one (macOS, the BSDs).
//
// shortcut: Linux has a birth time only through statx(2), which aggy does
// not reach (x/sys/unix is banned), so there the gate uses the change
// time of the content (mtime), which a process with a shell could set
// back; no M1 image or video child has a shell. Upgrade when the
// standard library exposes statx.
func born(fi fs.FileInfo) time.Time {
	if n, ok := safefs.FileBirth(fi); ok {
		return time.Unix(0, n)
	}
	return fi.ModTime()
}

// readRest is io.ReadAll; tests wrap it to see which files are read whole.
var readRest = io.ReadAll

// sniffLen is how many leading bytes sniff needs.
const sniffLen = 12

// sniff returns the media type of data from its magic bytes, or "".
func sniff(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return PNG
	case bytes.HasPrefix(data, []byte("\xff\xd8\xff")):
		return JPEG
	case len(data) >= 12 && string(data[4:8]) == "ftyp":
		// An ISO base media file (MP4, MOV, 3GP) starts with an ftyp box.
		return MP4
	}
	return ""
}

// gate passes each artifact through readArtifact and returns the ones
// that passed, at most MaxImages, a reason for each one refused, and how
// many the run made.
// A resumed session's folder holds the files of its earlier runs: with
// resumed, a file made before this run is skipped quietly (still never
// copied) and not counted as made.
func gate(arts []adapter.Artifact, started time.Time, resumed bool) (ok []media, refused []string, made int) {
	over := 0
	for _, a := range arts {
		if len(ok) == MaxImages {
			// Image turns use a plan's limits several times faster than
			// text, and a vendor may make more than it was asked for:
			// keep MaxImages, and only count the rest (Lstat, no read).
			fi, err := safefs.Lstat(a.Path)
			if resumed && err == nil && born(fi).Before(started.Add(-timeSlack)) {
				continue
			}
			made++
			over++
			continue
		}
		m, err := readArtifact(a, started)
		if resumed && errors.Is(err, errOld) {
			continue
		}
		made++
		if err != nil {
			refused = append(refused, err.Error())
			continue
		}
		ok = append(ok, m)
	}
	if over > 0 {
		refused = append(refused, fmt.Sprintf("%d more not kept: the cap is %d files per borrow", over, MaxImages))
	}
	return ok, refused, made
}
