package epub

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
)

const (
	maxEntries       = 10000
	maxDocumentBytes = 16 << 20
	maxArchiveBytes  = 512 << 20
	maxBlocks        = 150000
	maxWords         = 500000
)

type Document struct {
	Version   int       `json:"version"`
	Title     string    `json:"title"`
	Author    string    `json:"author"`
	Language  string    `json:"language"`
	WordCount int       `json:"word_count"`
	Chapters  []Chapter `json:"chapters"`
}

type Chapter struct {
	ID      string  `json:"id"`
	Ordinal int     `json:"ordinal"`
	Title   string  `json:"title"`
	Blocks  []Block `json:"blocks"`
}

type Block struct {
	ID        string     `json:"id"`
	Kind      string     `json:"kind"`
	Text      string     `json:"text"`
	Sentences []Sentence `json:"sentences"`
}

type Sentence struct {
	ID    string   `json:"id"`
	Text  string   `json:"text"`
	Words []string `json:"words"`
}

func (d Document) HintPrompt() string {
	var parts []string
	if d.Title != "" {
		parts = append(parts, d.Title)
	}
	if d.Author != "" {
		parts = append(parts, d.Author)
	}
	for _, chapter := range d.Chapters {
		if chapter.Title != "" && len(parts) < 8 {
			parts = append(parts, chapter.Title)
		}
	}
	prompt := "Audiobook title and chapter/name hints: " + strings.Join(parts, "; ")
	runes := []rune(prompt)
	if len(runes) > 420 {
		prompt = string(runes[:420])
	}
	return prompt
}

type rootFile struct {
	FullPath string `xml:"full-path,attr"`
}

type containerDoc struct {
	Rootfiles []rootFile `xml:"rootfiles>rootfile"`
}

type manifestItem struct {
	ID         string `xml:"id,attr"`
	Href       string `xml:"href,attr"`
	MediaType  string `xml:"media-type,attr"`
	Properties string `xml:"properties,attr"`
}

type spineItem struct {
	IDRef  string `xml:"idref,attr"`
	Linear string `xml:"linear,attr"`
}

type packageDoc struct {
	Metadata struct {
		Titles    []string `xml:"title"`
		Creators  []string `xml:"creator"`
		Languages []string `xml:"language"`
	} `xml:"metadata"`
	Manifest struct {
		Items []manifestItem `xml:"item"`
	} `xml:"manifest"`
	Spine struct {
		Items []spineItem `xml:"itemref"`
	} `xml:"spine"`
}

// ParseFile reads the EPUB package and extracts only spine-ordered XHTML text.
// It does not render publisher CSS, follow links, or access external resources.
func ParseFile(filename string) (Document, error) {
	zr, err := zip.OpenReader(filename)
	if err != nil {
		return Document{}, fmt.Errorf("invalid EPUB archive")
	}
	defer zr.Close()
	if len(zr.File) == 0 || len(zr.File) > maxEntries {
		return Document{}, fmt.Errorf("EPUB contains too many files")
	}

	files := make(map[string]*zip.File, len(zr.File))
	var total uint64
	for _, f := range zr.File {
		name, err := safeEntryName(f.Name)
		if err != nil {
			return Document{}, fmt.Errorf("EPUB contains an unsafe path")
		}
		if _, exists := files[name]; exists {
			return Document{}, fmt.Errorf("EPUB contains duplicate paths")
		}
		if f.Mode()&os.ModeSymlink != 0 {
			return Document{}, fmt.Errorf("EPUB contains a symbolic link")
		}
		if f.UncompressedSize64 > maxArchiveBytes || total > maxArchiveBytes-f.UncompressedSize64 {
			return Document{}, fmt.Errorf("EPUB expands beyond the allowed size")
		}
		total += f.UncompressedSize64
		files[name] = f
	}

	containerBytes, err := readZipFile(files["META-INF/container.xml"], 1<<20)
	if err != nil {
		return Document{}, fmt.Errorf("EPUB container metadata is invalid")
	}
	var container containerDoc
	if err := decodeXML(containerBytes, &container); err != nil || len(container.Rootfiles) == 0 {
		return Document{}, fmt.Errorf("EPUB container metadata is invalid")
	}
	opfPath, err := safeEntryName(container.Rootfiles[0].FullPath)
	if err != nil {
		return Document{}, fmt.Errorf("EPUB package path is invalid")
	}
	opfBytes, err := readZipFile(files[opfPath], 4<<20)
	if err != nil {
		return Document{}, fmt.Errorf("EPUB package document is missing")
	}
	var pkg packageDoc
	if err := decodeXML(opfBytes, &pkg); err != nil || len(pkg.Spine.Items) == 0 {
		return Document{}, fmt.Errorf("EPUB package document is invalid")
	}

	manifest := make(map[string]manifestItem, len(pkg.Manifest.Items))
	for _, item := range pkg.Manifest.Items {
		if item.ID != "" {
			manifest[item.ID] = item
		}
	}
	doc := Document{Version: 1}
	if len(pkg.Metadata.Titles) > 0 {
		doc.Title = cleanText(pkg.Metadata.Titles[0])
	}
	if len(pkg.Metadata.Creators) > 0 {
		doc.Author = cleanText(pkg.Metadata.Creators[0])
	}
	if len(pkg.Metadata.Languages) > 0 {
		doc.Language = cleanText(pkg.Metadata.Languages[0])
	}

	for _, itemRef := range pkg.Spine.Items {
		item, ok := manifest[itemRef.IDRef]
		if !ok || !isXHTML(item.MediaType) || hasProperty(item.Properties, "nav") ||
			strings.EqualFold(itemRef.Linear, "no") {
			continue
		}
		contentPath, err := resolvePackageHref(opfPath, item.Href)
		if err != nil {
			return Document{}, fmt.Errorf("EPUB spine has an invalid content path")
		}
		content, err := readZipFile(files[contentPath], maxDocumentBytes)
		if err != nil {
			return Document{}, fmt.Errorf("EPUB spine content is missing or too large")
		}
		chapter, err := extractChapter(content, len(doc.Chapters), contentPath)
		if err != nil {
			return Document{}, fmt.Errorf("EPUB spine content is not valid XHTML")
		}
		if len(chapter.Blocks) == 0 {
			continue
		}
		for _, block := range chapter.Blocks {
			doc.WordCount += len(blockWords(block))
			if doc.WordCount > maxWords {
				return Document{}, fmt.Errorf("EPUB contains too much text")
			}
		}
		doc.Chapters = append(doc.Chapters, chapter)
		if countBlocks(doc.Chapters) > maxBlocks {
			return Document{}, fmt.Errorf("EPUB contains too many text sections")
		}
	}
	if len(doc.Chapters) == 0 || doc.WordCount == 0 {
		return Document{}, fmt.Errorf("EPUB has no readable text in its spine")
	}
	return doc, nil
}

func countBlocks(chapters []Chapter) int {
	n := 0
	for _, chapter := range chapters {
		n += len(chapter.Blocks)
	}
	return n
}

func blockWords(block Block) []string {
	var out []string
	for _, sentence := range block.Sentences {
		out = append(out, sentence.Words...)
	}
	return out
}

func safeEntryName(name string) (string, error) {
	if name == "" || strings.Contains(name, "\\") || strings.ContainsRune(name, '\x00') {
		return "", fmt.Errorf("invalid ZIP path")
	}
	name = strings.TrimSuffix(name, "/")
	if name == "" || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("invalid ZIP path")
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("invalid ZIP path")
		}
	}
	clean := path.Clean(name)
	if clean != name || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("invalid ZIP path")
	}
	return clean, nil
}

func resolvePackageHref(opfPath, href string) (string, error) {
	u, err := parsePackageURL(href)
	if err != nil {
		return "", err
	}
	unescaped, err := decodePath(u)
	if err != nil {
		return "", err
	}
	joined := path.Clean(path.Join(path.Dir(opfPath), unescaped))
	if joined == "." || joined == ".." || strings.HasPrefix(joined, "../") || strings.HasPrefix(joined, "/") {
		return "", fmt.Errorf("path escapes EPUB")
	}
	return joined, nil
}

type packageURL struct {
	path string
}

func parsePackageURL(raw string) (packageURL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.Contains(raw, "\\") {
		return packageURL{}, fmt.Errorf("invalid package URL")
	}
	if strings.ContainsAny(raw, "?#") {
		return packageURL{}, fmt.Errorf("external or fragmented resource")
	}
	if strings.HasPrefix(raw, "/") || strings.Contains(raw, ":") {
		return packageURL{}, fmt.Errorf("absolute package URL")
	}
	return packageURL{path: raw}, nil
}

func decodePath(u packageURL) (string, error) {
	var out strings.Builder
	for i := 0; i < len(u.path); i++ {
		if u.path[i] == '%' {
			if i+2 >= len(u.path) {
				return "", fmt.Errorf("bad escape")
			}
			value, err := strconv.ParseUint(u.path[i+1:i+3], 16, 8)
			if err != nil {
				return "", fmt.Errorf("bad escape")
			}
			out.WriteByte(byte(value))
			i += 2
		} else {
			out.WriteByte(u.path[i])
		}
	}
	return out.String(), nil
}

func readZipFile(f *zip.File, limit uint64) ([]byte, error) {
	if f == nil || f.UncompressedSize64 > limit {
		return nil, fmt.Errorf("ZIP entry unavailable")
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, int64(limit)+1))
	if err != nil || uint64(len(data)) > limit {
		return nil, fmt.Errorf("ZIP entry too large")
	}
	return data, nil
}

func decodeXML(data []byte, dst any) error {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.CharsetReader = charsetReader
	return decoder.Decode(dst)
}

func charsetReader(label string, input io.Reader) (io.Reader, error) {
	name := strings.ToLower(strings.TrimSpace(label))
	if name == "utf-8" || name == "utf8" || name == "us-ascii" || name == "ascii" {
		return input, nil
	}
	data, err := io.ReadAll(io.LimitReader(input, maxDocumentBytes+1))
	if err != nil || len(data) > maxDocumentBytes {
		return nil, fmt.Errorf("XML encoding is too large")
	}
	switch name {
	case "utf-16", "utf-16le", "utf-16be":
		little := strings.HasSuffix(name, "le") || name == "utf-16" && bytes.HasPrefix(data, []byte{0xff, 0xfe})
		if bytes.HasPrefix(data, []byte{0xff, 0xfe}) || bytes.HasPrefix(data, []byte{0xfe, 0xff}) {
			if data[0] == 0xfe {
				little = false
			}
			data = data[2:]
		}
		if len(data)%2 != 0 {
			return nil, fmt.Errorf("invalid UTF-16")
		}
		units := make([]uint16, len(data)/2)
		for i := range units {
			if little {
				units[i] = binary.LittleEndian.Uint16(data[i*2:])
			} else {
				units[i] = binary.BigEndian.Uint16(data[i*2:])
			}
		}
		return bytes.NewReader([]byte(string(utf16.Decode(units)))), nil
	case "iso-8859-1", "latin1", "windows-1252":
		runes := make([]rune, len(data))
		for i, b := range data {
			runes[i] = rune(b)
		}
		return strings.NewReader(string(runes)), nil
	default:
		return nil, fmt.Errorf("unsupported XML encoding")
	}
}

func isXHTML(mediaType string) bool {
	return strings.EqualFold(strings.TrimSpace(strings.Split(mediaType, ";")[0]), "application/xhtml+xml") ||
		strings.EqualFold(strings.TrimSpace(strings.Split(mediaType, ";")[0]), "text/html")
}

func hasProperty(properties, property string) bool {
	for _, v := range strings.Fields(properties) {
		if v == property {
			return true
		}
	}
	return false
}

type blockBuilder struct {
	kind  string
	depth int
	text  strings.Builder
}

func extractChapter(data []byte, ordinal int, contentPath string) (Chapter, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.CharsetReader = charsetReader
	depth := 0
	skipDepth := 0
	var active *blockBuilder
	blocks := []Block{}
	var firstHeading string
	flush := func() {
		if active == nil {
			return
		}
		text := cleanText(active.text.String())
		if text != "" {
			blockID := fmt.Sprintf("ch%04d-b%05d", ordinal+1, len(blocks)+1)
			sentences := splitSentences(text, blockID)
			if len(sentences) > 0 {
				blocks = append(blocks, Block{ID: blockID, Kind: active.kind, Text: text, Sentences: sentences})
				if firstHeading == "" && strings.HasPrefix(active.kind, "h") {
					firstHeading = text
				}
			}
		}
		active = nil
	}
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Chapter{}, err
		}
		switch t := token.(type) {
		case xml.StartElement:
			depth++
			name := strings.ToLower(t.Name.Local)
			if skipDepth > 0 {
				skipDepth++
				continue
			}
			if isSkippedElement(name) {
				flush()
				skipDepth = 1
				continue
			}
			if isBlockElement(name) {
				flush()
				active = &blockBuilder{kind: name, depth: depth}
			} else if name == "br" && active != nil {
				active.text.WriteByte(' ')
			}
		case xml.CharData:
			if skipDepth == 0 && active != nil {
				active.text.Write([]byte(t))
				active.text.WriteByte(' ')
			}
		case xml.EndElement:
			if skipDepth > 0 {
				skipDepth--
				depth--
				continue
			}
			if active != nil && active.depth == depth {
				flush()
			}
			depth--
		}
	}
	flush()
	if len(blocks) == 0 {
		return Chapter{}, nil
	}
	title := firstHeading
	if title == "" {
		title = strings.TrimSuffix(path.Base(contentPath), path.Ext(contentPath))
		title = strings.ReplaceAll(strings.ReplaceAll(title, "_", " "), "-", " ")
	}
	return Chapter{ID: fmt.Sprintf("ch%04d", ordinal+1), Ordinal: ordinal, Title: title, Blocks: blocks}, nil
}

func isSkippedElement(name string) bool {
	switch name {
	case "head", "nav", "script", "style", "svg", "math", "noscript":
		return true
	default:
		return false
	}
}

func isBlockElement(name string) bool {
	if name == "p" || name == "li" || name == "blockquote" || name == "pre" ||
		name == "dt" || name == "dd" || name == "figcaption" || name == "td" || name == "th" {
		return true
	}
	return len(name) == 2 && name[0] == 'h' && name[1] >= '1' && name[1] <= '6'
}

func cleanText(value string) string {
	value = strings.ToValidUTF8(value, "")
	return strings.Join(strings.Fields(value), " ")
}

func splitSentences(text, blockID string) []Sentence {
	runes := []rune(text)
	var out []Sentence
	start := 0
	flush := func(end int) {
		line := strings.TrimSpace(string(runes[start:end]))
		if line == "" {
			return
		}
		words := Tokenize(line)
		if len(words) > 0 {
			out = append(out, Sentence{
				ID: fmt.Sprintf("%s-s%03d", blockID, len(out)+1), Text: line, Words: words,
			})
		}
	}
	for i, r := range runes {
		if r != '.' && r != '?' && r != '!' && r != '…' && r != '。' && r != '！' && r != '？' {
			continue
		}
		nextIsBoundary := i+1 == len(runes) || unicode.IsSpace(runes[i+1]) ||
			runes[i+1] == '"' || runes[i+1] == '\'' || runes[i+1] == '”' || runes[i+1] == '’' ||
			runes[i+1] == ')' || runes[i+1] == ']'
		if !nextIsBoundary || isAbbreviation(runes, start, i) {
			continue
		}
		flush(i + 1)
		start = i + 1
	}
	flush(len(runes))
	return out
}

func isAbbreviation(runes []rune, start, dot int) bool {
	if runes[dot] != '.' {
		return false
	}
	wordStart := dot
	for wordStart > start && unicode.IsLetter(runes[wordStart-1]) {
		wordStart--
	}
	word := strings.ToLower(string(runes[wordStart:dot]))
	switch word {
	case "mr", "mrs", "ms", "dr", "prof", "st", "vs", "etc", "e.g", "i.e":
		return true
	}
	return len([]rune(word)) == 1 && len(word) == 1
}

// Tokenize returns display-preserving word tokens for alignment; punctuation
// remains attached so the ebook can render the author's text, not the ASR text.
func Tokenize(text string) []string {
	runes := []rune(text)
	var out []string
	var current, prefix strings.Builder
	active, trailingPunctuation := false, false
	flush := func() {
		if active {
			out = append(out, current.String())
		}
		current.Reset()
		active, trailingPunctuation = false, false
	}
	for i, r := range runes {
		wordChar := unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r)
		if wordChar {
			if trailingPunctuation {
				flush()
			}
			if !active {
				current.WriteString(prefix.String())
				prefix.Reset()
				active = true
			}
			current.WriteRune(r)
			continue
		}
		if active && isWordJoiner(r) && i+1 < len(runes) &&
			(unicode.IsLetter(runes[i+1]) || unicode.IsDigit(runes[i+1])) {
			current.WriteRune(r)
			continue
		}
		if active && (unicode.IsPunct(r) || unicode.IsSymbol(r)) {
			current.WriteRune(r)
			trailingPunctuation = true
			continue
		}
		if !active && isOpeningPunctuation(r) {
			prefix.WriteRune(r)
			continue
		}
		if unicode.IsSpace(r) {
			flush()
		} else if active {
			flush()
		}
	}
	flush()
	return out
}

func isOpeningPunctuation(r rune) bool {
	switch r {
	case '"', '\'', '“', '‘', '(', '[', '{', '¡', '¿':
		return true
	default:
		return false
	}
}

func isWordJoiner(r rune) bool {
	switch r {
	case '\'', '’', 'ʼ', '-', '‐', '‑':
		return true
	default:
		return false
	}
}
