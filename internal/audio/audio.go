// Package audio reads metadata from a clean M4B audiobook: title, author,
// narrator, duration, embedded chapters, and cover art. It is a sibling to
// internal/epub and internal/comic with the same shape (Read/CoverImage), so the
// catalog and cover cache treat an audiobook like any other book.
//
// M4B is an MP4/ISO-BMFF container (the same box structure as .mp4/.m4a). This
// package parses the boxes in PURE GO: it never shells out to ffmpeg/ffprobe.
// That is deliberate and matches the project invariant that the Go side stays
// pure (ffmpeg lives only in the audiobook sidecar, see internal/audible). All
// the metadata we need sits in the file's "moov" box, which a clean -c-copy from
// a .aax carries through unchanged:
//
//	moov/mvhd                 -> duration (timescale + duration fields)
//	moov/udta/meta/ilst       -> iTunes tags (©nam title, ©ART author,
//	                             aART narrator/album-artist, ©day year, covr art)
//	moov/udta/chpl            -> Nero chapter list (flat start-time + title)
//
// We read the Nero "chpl" chapter box (a flat list) rather than the QuickTime
// chapter text track, because ffmpeg writes both and chpl is unambiguous to
// parse. See AUDIOBOOK_SUPPORT.md.
package audio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxMoovSize caps how many bytes of a "moov" box we will buffer, a guard
// against a malformed or hostile file declaring an enormous box. A real
// audiobook's moov (tags + chapters + sample tables) is well under this; the
// audio samples (mdat) are never read here.
const maxMoovSize = 64 << 20 // 64 MiB

// Chapter is one entry in an audiobook's chapter list.
type Chapter struct {
	Title string
	Start float64 // seconds from the start of the book
}

// Metadata is what the catalog needs from an audiobook. Field names mirror
// epub.Metadata so the same upsert path can store either; audio leaves the
// book-only fields it has no source for (e.g. Series) empty. Narrator and
// Chapters are audio-specific and ignored by the shared upsert, but are used by
// the player.
type Metadata struct {
	Title       string
	Authors     []string
	Narrator    string
	Series      string
	SeriesIndex float64
	Language    string
	Publisher   string
	Description string
	Published   string            // year, if known
	Identifiers map[string]string // always empty for audio; present for symmetry
	HasCover    bool              // true when a covr atom is present
	Duration    float64           // total length in seconds
	Chapters    []Chapter
}

// Read parses a clean .m4b and returns its metadata. It walks only the moov box;
// the audio data (mdat) is never read, so this is cheap even for a multi-hundred-
// MB book.
func Read(m4bPath string) (*Metadata, error) {
	f, err := os.Open(m4bPath)
	if err != nil {
		return nil, fmt.Errorf("open m4b: %w", err)
	}
	defer func() { _ = f.Close() }()

	moov, err := readMoov(f)
	if err != nil {
		return nil, err
	}

	m := &Metadata{Identifiers: map[string]string{}}
	if mvhd := findBox(moov, "mvhd"); mvhd != nil {
		m.Duration = parseMvhdDuration(mvhd)
	}
	if udta := findBox(moov, "udta"); udta != nil {
		if ilst := findPath(udta, "meta", "ilst"); ilst != nil {
			applyIlst(m, ilst)
		}
		if chpl := findBox(udta, "chpl"); chpl != nil {
			m.Chapters = parseChpl(chpl)
		}
	}
	// Chapters can live in two places. ffmpeg's muxer writes a Nero "chpl" box
	// (handled above); real Audible files instead carry a QuickTime chapter
	// TEXT TRACK referenced by the audio track's tref/chap. When there's no
	// chpl, fall back to reading that track (it needs the file handle to read
	// the title samples out of mdat).
	if len(m.Chapters) == 0 {
		if chaps, err := readChapterTrack(f, moov); err == nil {
			m.Chapters = chaps
		}
	}
	// Title always falls back to the filename (an audiobook with no ©nam tag
	// still wants a sensible name), matching comic.Read's behavior.
	if m.Title == "" {
		m.Title = titleFromFilename(m4bPath)
	}
	return m, nil
}

// CoverImage returns the embedded cover art (the ilst "covr" atom) and its MIME
// type, or an error if the file has none. Shape mirrors epub.CoverImage and
// comic.CoverImage so coverImageFor can dispatch uniformly.
func CoverImage(m4bPath string) ([]byte, string, error) {
	f, err := os.Open(m4bPath)
	if err != nil {
		return nil, "", fmt.Errorf("open m4b: %w", err)
	}
	defer func() { _ = f.Close() }()

	moov, err := readMoov(f)
	if err != nil {
		return nil, "", err
	}
	udta := findBox(moov, "udta")
	if udta == nil {
		return nil, "", errors.New("no cover: file has no udta box")
	}
	ilst := findPath(udta, "meta", "ilst")
	if ilst == nil {
		return nil, "", errors.New("no cover: file has no ilst tags")
	}
	covr := findBox(ilst, "covr")
	if covr == nil {
		return nil, "", errors.New("no cover: file has no covr atom")
	}
	data, typeFlags := ilstData(covr)
	if data == nil {
		return nil, "", errors.New("no cover: covr atom has no data")
	}
	// covr's data type flag: 13 = JPEG, 14 = PNG. Sniff as a fallback.
	mime := "image/jpeg"
	switch typeFlags {
	case 14:
		mime = "image/png"
	case 13:
		mime = "image/jpeg"
	default:
		if bytes.HasPrefix(data, []byte("\x89PNG")) {
			mime = "image/png"
		}
	}
	return data, mime, nil
}

// readMoov scans the top-level box list and returns the moov box's payload
// (the bytes inside it, excluding its own 8-byte header). It reads box headers
// sequentially and seeks past the (large) mdat without buffering it.
func readMoov(rs io.ReadSeeker) ([]byte, error) {
	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	for {
		size, typ, headerLen, err := readBoxHeader(rs)
		if err == io.EOF {
			return nil, errors.New("no moov box (not a valid m4b?)")
		}
		if err != nil {
			return nil, err
		}
		payloadLen := size - int64(headerLen)
		if payloadLen < 0 {
			return nil, fmt.Errorf("corrupt box %q: size %d < header %d", typ, size, headerLen)
		}
		if typ == "moov" {
			if payloadLen > maxMoovSize {
				return nil, fmt.Errorf("moov box too large: %d bytes", payloadLen)
			}
			buf := make([]byte, payloadLen)
			if _, err := io.ReadFull(rs, buf); err != nil {
				return nil, fmt.Errorf("read moov: %w", err)
			}
			return buf, nil
		}
		if _, err := rs.Seek(payloadLen, io.SeekCurrent); err != nil {
			return nil, fmt.Errorf("seek past %q: %w", typ, err)
		}
	}
}

// readBoxHeader reads one ISO-BMFF box header from rs and returns the box's
// total size (header + payload), its 4-char type, and the header length (8, or
// 16 for the 64-bit largesize form). A declared size of 0 means "to EOF", which
// we reject here because moov is always sized.
func readBoxHeader(rs io.ReadSeeker) (size int64, typ string, headerLen int, err error) {
	var hdr [8]byte
	if _, err = io.ReadFull(rs, hdr[:]); err != nil {
		return 0, "", 0, err
	}
	size32 := binary.BigEndian.Uint32(hdr[0:4])
	typ = string(hdr[4:8])
	headerLen = 8
	switch size32 {
	case 1: // 64-bit largesize follows
		var large [8]byte
		if _, err = io.ReadFull(rs, large[:]); err != nil {
			return 0, "", 0, err
		}
		size = int64(binary.BigEndian.Uint64(large[:]))
		headerLen = 16
	case 0:
		return 0, "", 0, fmt.Errorf("box %q has open-ended size, unsupported at top level", typ)
	default:
		size = int64(size32)
	}
	if size < int64(headerLen) {
		return 0, "", 0, fmt.Errorf("box %q declares size %d < header %d", typ, size, headerLen)
	}
	return size, typ, headerLen, nil
}

// walkBoxes iterates the child boxes within an in-memory payload, calling fn for
// each with its type and payload slice. It stops on the first malformed header.
// "Full boxes" (those with a version+flags prefix, like meta) are handled by
// callers stripping that prefix before walking; see findPath.
func walkBoxes(payload []byte, fn func(typ string, body []byte) bool) {
	for len(payload) >= 8 {
		size := binary.BigEndian.Uint32(payload[0:4])
		typ := string(payload[4:8])
		hdr := 8
		var boxSize int
		switch size {
		case 1:
			if len(payload) < 16 {
				return
			}
			boxSize = int(binary.BigEndian.Uint64(payload[8:16]))
			hdr = 16
		case 0:
			boxSize = len(payload) // to end of this container
		default:
			boxSize = int(size)
		}
		if boxSize < hdr || boxSize > len(payload) {
			return // truncated or corrupt: stop rather than read out of bounds
		}
		if !fn(typ, payload[hdr:boxSize]) {
			return
		}
		payload = payload[boxSize:]
	}
}

// findBox returns the payload of the first direct child box of the given type,
// or nil if absent.
func findBox(payload []byte, typ string) []byte {
	var found []byte
	walkBoxes(payload, func(t string, body []byte) bool {
		if t == typ {
			found = body
			return false
		}
		return true
	})
	return found
}

// findPath descends a chain of nested boxes. The "meta" box is a FullBox: it has
// a 4-byte version+flags prefix before its children, which we skip so the inner
// walk sees real boxes (ilst, hdlr, ...).
func findPath(payload []byte, path ...string) []byte {
	cur := payload
	for _, p := range path {
		child := findBox(cur, p)
		if child == nil {
			return nil
		}
		if p == "meta" {
			if len(child) < 4 {
				return nil
			}
			child = child[4:] // strip version+flags of the meta FullBox
		}
		cur = child
	}
	return cur
}

// parseMvhdDuration reads the movie duration from an mvhd box. mvhd is a FullBox;
// the field layout depends on its version: v0 uses 32-bit times, v1 uses 64-bit.
// Layout after the 4-byte version+flags:
//
//	v0: creation(4) modification(4) timescale(4) duration(4)
//	v1: creation(8) modification(8) timescale(4) duration(8)
func parseMvhdDuration(mvhd []byte) float64 {
	if len(mvhd) < 4 {
		return 0
	}
	version := mvhd[0]
	var timescale uint32
	var duration uint64
	switch version {
	case 1:
		if len(mvhd) < 4+8+8+4+8 {
			return 0
		}
		timescale = binary.BigEndian.Uint32(mvhd[4+16 : 4+20])
		duration = binary.BigEndian.Uint64(mvhd[4+20 : 4+28])
	default: // version 0
		if len(mvhd) < 4+4+4+4+4 {
			return 0
		}
		timescale = binary.BigEndian.Uint32(mvhd[4+8 : 4+12])
		duration = uint64(binary.BigEndian.Uint32(mvhd[4+12 : 4+16]))
	}
	if timescale == 0 {
		return 0
	}
	return float64(duration) / float64(timescale)
}

// applyIlst pulls the iTunes-style tags we care about from an ilst box. Each
// child atom (named by a 4-byte code, e.g. ©nam) wraps a "data" box holding the
// value. The ©-prefixed codes use the 0xA9 byte.
func applyIlst(m *Metadata, ilst []byte) {
	const cr = "\xa9" // 0xA9, the "©" tag prefix
	walkBoxes(ilst, func(typ string, body []byte) bool {
		val, _ := ilstData(body)
		if val == nil {
			return true
		}
		switch typ {
		case cr + "nam":
			m.Title = string(val)
		case cr + "ART":
			if s := strings.TrimSpace(string(val)); s != "" {
				m.Authors = splitPeople(s)
			}
		case "aART": // album artist: Audible stores the narrator here
			m.Narrator = strings.TrimSpace(string(val))
		case cr + "day":
			m.Published = yearOf(string(val))
		case cr + "pub":
			m.Publisher = strings.TrimSpace(string(val))
		case "desc", cr + "des":
			if m.Description == "" {
				m.Description = strings.TrimSpace(string(val))
			}
		case cr + "alb":
			if m.Series == "" {
				m.Series = strings.TrimSpace(string(val))
			}
		case "covr":
			m.HasCover = true
		}
		return true
	})
}

// ilstData unwraps an ilst atom's inner "data" box, returning the raw value
// bytes and the 4-byte type indicator (1 = UTF-8 text, 13 = JPEG, 14 = PNG).
// The data box layout is: size(4) 'data'(4) typeIndicator(4) locale(4) value...
func ilstData(atom []byte) (value []byte, typeFlags uint32) {
	data := findBox(atom, "data")
	if len(data) < 8 {
		return nil, 0
	}
	typeFlags = binary.BigEndian.Uint32(data[0:4])
	return data[8:], typeFlags
}

// parseChpl reads a Nero "chpl" chapter box. Layout:
//
//	version(1) flags(3) reserved(4) count(1) then per chapter:
//	  start(8, in 100-ns units) titleLen(1) title(titleLen bytes, UTF-8)
//
// (Some muxers use a 4-byte count; ffmpeg writes the reserved(4)+count(1) form,
// which is what a clean .aax conversion produces.)
func parseChpl(chpl []byte) []Chapter {
	if len(chpl) < 9 {
		return nil
	}
	// version(1) flags(3) reserved(4) count(1)
	count := int(chpl[8])
	p := chpl[9:]
	chapters := make([]Chapter, 0, count)
	for i := 0; i < count; i++ {
		if len(p) < 9 {
			break
		}
		start100ns := binary.BigEndian.Uint64(p[0:8])
		titleLen := int(p[8])
		p = p[9:]
		if len(p) < titleLen {
			break
		}
		chapters = append(chapters, Chapter{
			Title: string(p[:titleLen]),
			Start: float64(start100ns) / 1e7, // 100-ns units -> seconds
		})
		p = p[titleLen:]
	}
	return chapters
}

// readChapterTrack reads chapters from a QuickTime chapter text track, the form
// real Audible files use. It locates the audio track's tref/chap reference,
// finds the referenced text track, and turns its samples into chapters:
//
//	stts (sample-table time-to-sample) -> cumulative deltas / mdhd timescale
//	                                      = each chapter's start time
//	stsz + stco + stsc                 -> where each sample's bytes are in mdat
//	each text sample                   -> uint16 length prefix + UTF-8 title
//
// Returns an error (not nil chapters) when there is no chapter track, so the
// caller can leave Chapters empty.
func readChapterTrack(rs io.ReadSeeker, moov []byte) ([]Chapter, error) {
	chapID, ok := chapterTrackID(moov)
	if !ok {
		return nil, errors.New("no tref/chap reference")
	}
	trak := trackByID(moov, chapID)
	if trak == nil {
		return nil, fmt.Errorf("chapter track id %d not found", chapID)
	}
	mdia := findBox(trak, "mdia")
	if mdia == nil {
		return nil, errors.New("chapter track has no mdia")
	}
	timescale := mdhdTimescale(findBox(mdia, "mdhd"))
	if timescale == 0 {
		return nil, errors.New("chapter track has zero timescale")
	}
	stbl := findPath(mdia, "minf", "stbl")
	if stbl == nil {
		return nil, errors.New("chapter track has no stbl")
	}
	deltas := parseStts(findBox(stbl, "stts"))
	sizes := parseStsz(findBox(stbl, "stsz"))
	offsets := sampleOffsets(stbl, len(sizes))
	if len(sizes) == 0 || len(offsets) < len(sizes) {
		return nil, errors.New("chapter track has no usable sample table")
	}

	chapters := make([]Chapter, 0, len(sizes))
	var t uint64
	for i := range sizes {
		raw := make([]byte, sizes[i])
		if _, err := rs.Seek(int64(offsets[i]), io.SeekStart); err != nil {
			break
		}
		if _, err := io.ReadFull(rs, raw); err != nil {
			break
		}
		title := decodeTextSample(raw)
		start := float64(t) / float64(timescale)
		if i < len(deltas) {
			t += deltas[i]
		}
		if title == "" {
			continue
		}
		chapters = append(chapters, Chapter{Title: title, Start: start})
	}
	if len(chapters) == 0 {
		return nil, errors.New("chapter track yielded no chapters")
	}
	return chapters, nil
}

// chapterTrackID returns the track ID referenced by the FIRST track that has a
// tref/chap box (the audio track points at its chapter track). The chap box is a
// list of 32-bit track IDs; we take the first.
func chapterTrackID(moov []byte) (uint32, bool) {
	var id uint32
	var found bool
	walkBoxes(moov, func(typ string, body []byte) bool {
		if typ != "trak" {
			return true
		}
		tref := findBox(body, "tref")
		if tref == nil {
			return true
		}
		chap := findBox(tref, "chap")
		if len(chap) < 4 {
			return true
		}
		id = binary.BigEndian.Uint32(chap[0:4])
		found = true
		return false
	})
	return id, found
}

// trackByID returns the trak box whose tkhd track_id matches id.
func trackByID(moov []byte, id uint32) []byte {
	var match []byte
	walkBoxes(moov, func(typ string, body []byte) bool {
		if typ != "trak" {
			return true
		}
		if tkhd := findBox(body, "tkhd"); trackID(tkhd) == id {
			match = body
			return false
		}
		return true
	})
	return match
}

// trackID reads the track_id from a tkhd FullBox. Field offset depends on
// version: v0 has 32-bit creation/modification times, v1 has 64-bit.
func trackID(tkhd []byte) uint32 {
	if len(tkhd) < 4 {
		return 0
	}
	if tkhd[0] == 1 {
		if len(tkhd) < 4+8+8+4 {
			return 0
		}
		return binary.BigEndian.Uint32(tkhd[4+16 : 4+20])
	}
	if len(tkhd) < 4+4+4+4 {
		return 0
	}
	return binary.BigEndian.Uint32(tkhd[4+8 : 4+12])
}

// mdhdTimescale reads the media timescale from an mdhd FullBox (v0 = 32-bit
// times, v1 = 64-bit).
func mdhdTimescale(mdhd []byte) uint32 {
	if len(mdhd) < 4 {
		return 0
	}
	if mdhd[0] == 1 {
		if len(mdhd) < 4+8+8+4 {
			return 0
		}
		return binary.BigEndian.Uint32(mdhd[4+16 : 4+20])
	}
	if len(mdhd) < 4+4+4+4 {
		return 0
	}
	return binary.BigEndian.Uint32(mdhd[4+8 : 4+12])
}

// parseStts expands a time-to-sample box into a per-sample duration slice.
// Layout: version+flags(4) entryCount(4) then [sampleCount(4) sampleDelta(4)]*.
func parseStts(stts []byte) []uint64 {
	if len(stts) < 8 {
		return nil
	}
	n := binary.BigEndian.Uint32(stts[4:8])
	var deltas []uint64
	p := stts[8:]
	for i := uint32(0); i < n; i++ {
		if len(p) < 8 {
			break
		}
		count := binary.BigEndian.Uint32(p[0:4])
		delta := binary.BigEndian.Uint32(p[4:8])
		p = p[8:]
		// Guard against an absurd count (corrupt box) blowing up memory.
		if count > 1<<20 {
			break
		}
		for j := uint32(0); j < count; j++ {
			deltas = append(deltas, uint64(delta))
		}
	}
	return deltas
}

// parseStsz returns each sample's byte size. Layout: version+flags(4)
// sampleSize(4) sampleCount(4) then, if sampleSize==0, sampleCount*size(4).
func parseStsz(stsz []byte) []uint32 {
	if len(stsz) < 12 {
		return nil
	}
	uniform := binary.BigEndian.Uint32(stsz[4:8])
	count := binary.BigEndian.Uint32(stsz[8:12])
	if count > 1<<20 {
		return nil
	}
	if uniform != 0 {
		out := make([]uint32, count)
		for i := range out {
			out[i] = uniform
		}
		return out
	}
	out := make([]uint32, 0, count)
	p := stsz[12:]
	for i := uint32(0); i < count; i++ {
		if len(p) < 4 {
			break
		}
		out = append(out, binary.BigEndian.Uint32(p[0:4]))
		p = p[4:]
	}
	return out
}

// sampleOffsets returns the absolute file offset of each of the first nSamples
// samples, resolving stsc (sample-to-chunk) against stco/co64 (chunk offsets).
// Chapter text tracks are small; this handles the general layout but caps work
// at nSamples.
func sampleOffsets(stbl []byte, nSamples int) []uint64 {
	chunkOffsets := parseChunkOffsets(stbl)
	if len(chunkOffsets) == 0 || nSamples == 0 {
		return nil
	}
	samplesPerChunk := parseStsc(findBox(stbl, "stsc"), len(chunkOffsets))
	sizes := parseStsz(findBox(stbl, "stsz"))

	offsets := make([]uint64, 0, nSamples)
	sample := 0
	for c := 0; c < len(chunkOffsets) && sample < nSamples; c++ {
		spc := 1
		if c < len(samplesPerChunk) {
			spc = samplesPerChunk[c]
		}
		pos := chunkOffsets[c]
		for s := 0; s < spc && sample < nSamples; s++ {
			offsets = append(offsets, pos)
			if sample < len(sizes) {
				pos += uint64(sizes[sample])
			}
			sample++
		}
	}
	return offsets
}

// parseChunkOffsets reads stco (32-bit) or co64 (64-bit) chunk offsets.
func parseChunkOffsets(stbl []byte) []uint64 {
	if stco := findBox(stbl, "stco"); len(stco) >= 8 {
		n := binary.BigEndian.Uint32(stco[4:8])
		if n > 1<<20 {
			return nil
		}
		out := make([]uint64, 0, n)
		p := stco[8:]
		for i := uint32(0); i < n && len(p) >= 4; i++ {
			out = append(out, uint64(binary.BigEndian.Uint32(p[0:4])))
			p = p[4:]
		}
		return out
	}
	if co64 := findBox(stbl, "co64"); len(co64) >= 8 {
		n := binary.BigEndian.Uint32(co64[4:8])
		if n > 1<<20 {
			return nil
		}
		out := make([]uint64, 0, n)
		p := co64[8:]
		for i := uint32(0); i < n && len(p) >= 8; i++ {
			out = append(out, binary.BigEndian.Uint64(p[0:8]))
			p = p[8:]
		}
		return out
	}
	return nil
}

// parseStsc expands the sample-to-chunk table into a per-chunk samples count for
// the first numChunks chunks. Layout: version+flags(4) entryCount(4) then
// [firstChunk(4) samplesPerChunk(4) descIndex(4)]* (1-based firstChunk, run-
// length encoded over chunks).
func parseStsc(stsc []byte, numChunks int) []int {
	if len(stsc) < 8 || numChunks == 0 {
		return nil
	}
	n := binary.BigEndian.Uint32(stsc[4:8])
	type entry struct{ first, spc uint32 }
	entries := make([]entry, 0, n)
	p := stsc[8:]
	for i := uint32(0); i < n && len(p) >= 12; i++ {
		entries = append(entries, entry{
			first: binary.BigEndian.Uint32(p[0:4]),
			spc:   binary.BigEndian.Uint32(p[4:8]),
		})
		p = p[12:]
	}
	if len(entries) == 0 {
		return nil
	}
	out := make([]int, numChunks)
	for c := 1; c <= numChunks; c++ {
		spc := entries[0].spc
		for _, e := range entries {
			if uint32(c) >= e.first {
				spc = e.spc
			} else {
				break
			}
		}
		out[c-1] = int(spc)
	}
	return out
}

// decodeTextSample extracts the title from a QuickTime text sample: a 16-bit
// big-endian length followed by that many UTF-8 bytes. Any trailing atoms
// (styling) after the text are ignored.
func decodeTextSample(raw []byte) string {
	if len(raw) < 2 {
		return ""
	}
	n := int(binary.BigEndian.Uint16(raw[0:2]))
	if n > len(raw)-2 {
		n = len(raw) - 2
	}
	return strings.TrimRight(string(raw[2:2+n]), "\x00")
}

// splitPeople splits a multi-author tag on the common separators. Audible
// usually has a single author string, but co-authored books use ", " or " & ".
func splitPeople(s string) []string {
	repl := strings.NewReplacer(" & ", "\n", ";", "\n", " and ", "\n")
	parts := strings.Split(repl.Replace(s), "\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// yearOf extracts a 4-digit year from a date tag that may be a full timestamp
// (e.g. "2024-03-01T00:00:00Z") or just a year.
func yearOf(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 4 {
		return s[:4]
	}
	return s
}

// titleFromFilename derives a title from the file's base name when no tag is
// present, mirroring comic.titleFromFilename.
func titleFromFilename(p string) string {
	base := filepath.Base(p)
	return strings.TrimSuffix(base, filepath.Ext(base))
}
