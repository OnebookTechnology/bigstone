// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
//

/*
Package multipart implements MIME multipart parsing, as defined in RFC
2046.

The implementation is sufficient for HTTP (RFC 2388) and the multipart
bodies generated by popular browsers.
*/
package server

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/textproto"
	"strings"
	"unicode"
)

var emptyParams = make(map[string]string)

// This constant needs to be at least 76 for this package to work correctly.
// This is because \r\n--separator_of_len_70- would fill the buffer and it
// wouldn't be safe to consume a single byte from it.
const peekBufferSize = 1 << 20 // 1M

// A Part represents a single part in a multipart body.
type Part struct {
	// The headers of the body, if any, with the keys canonicalized
	// in the same fashion that the Go http.Request headers are.
	// For example, "foo-bar" changes case to "Foo-Bar"
	//
	// As a special case, if the "Content-Transfer-Encoding" header
	// has a value of "quoted-printable", that header is instead
	// hidden from this map and the body is transparently decoded
	// during Read calls.
	Header textproto.MIMEHeader

	buffer    *bytes.Buffer
	mr        *Reader
	bytesRead int

	disposition       string
	dispositionParams map[string]string

	// r is either a reader directly reading from mr, or it's a
	// wrapper around such a reader, decoding the
	// Content-Transfer-Encoding
	r io.Reader
}

var multipartByReader = &multipart.Form{
	Value: make(map[string][]string),
	File:  make(map[string][]*multipart.FileHeader),
}

// FormName returns the name parameter if p has a Content-Disposition
// of type "form-data".  Otherwise it returns the empty string.
func (p *Part) FormName() string {
	// See http://tools.ietf.org/html/rfc2183 section 2 for EBNF
	// of Content-Disposition value format.
	if p.dispositionParams == nil {
		p.parseContentDisposition()
	}
	if p.disposition != "form-data" {
		return ""
	}
	return p.dispositionParams["name"]
}

// FileName returns the filename parameter of the Part's
// Content-Disposition header.
func (p *Part) FileName() string {
	if p.dispositionParams == nil {
		p.parseContentDisposition()
	}
	return p.dispositionParams["filename"]
}

func (p *Part) parseContentDisposition() {
	v := p.Header.Get("Content-Disposition")
	var err error
	p.disposition, p.dispositionParams, err = mime.ParseMediaType(v)
	if err != nil {
		p.dispositionParams = emptyParams
	}
}

// NewReader creates a new multipart Reader reading from r using the
// given MIME boundary.
//
// The boundary is usually obtained from the "boundary" parameter of
// the message's "Content-Type" header. Use mime.ParseMediaType to
// parse such headers.
func NewReader(r io.Reader, boundary string) *Reader {
	b := []byte("\r\n--" + boundary + "--")
	return &Reader{
		bufReader:        bufio.NewReaderSize(&stickyErrorReader{r: r}, peekBufferSize),
		nl:               b[:2],
		nlDashBoundary:   b[:len(b)-2],
		dashBoundaryDash: b[2:],
		dashBoundary:     b[2 : len(b)-2],
	}
}

// stickyErrorReader is an io.Reader which never calls Read on its
// underlying Reader once an error has been seen. (the io.Reader
// interface's contract promises nothing about the return values of
// Read calls after an error, yet this package does do multiple Reads
// after error)
type stickyErrorReader struct {
	r   io.Reader
	err error
}

func (r *stickyErrorReader) Read(p []byte) (n int, _ error) {
	if r.err != nil {
		return 0, r.err
	}
	n, r.err = r.r.Read(p)
	return n, r.err
}

func newPart(mr *Reader) (*Part, error) {
	bp := &Part{
		Header: make(map[string][]string),
		mr:     mr,
		buffer: new(bytes.Buffer),
	}
	if err := bp.populateHeaders(); err != nil {
		return nil, err
	}
	bp.r = partReader{bp}
	const cte = "Content-Transfer-Encoding"
	if bp.Header.Get(cte) == "quoted-printable" {
		bp.Header.Del(cte)
		bp.r = quotedprintable.NewReader(bp.r)
	}
	return bp, nil
}

func (bp *Part) populateHeaders() error {
	r := textproto.NewReader(bp.mr.bufReader)
	header, err := r.ReadMIMEHeader()
	if err == nil {
		bp.Header = header
	}
	return err
}

// Read reads the body of a part, after its headers and before the
// next part (if any) begins.
func (p *Part) Read(d []byte) (n int, err error) {
	return p.r.Read(d)
}

// partReader implements io.Reader by reading raw bytes directly from the
// wrapped *Part, without doing any Transfer-Encoding decoding.
type partReader struct {
	p *Part
}

func (pr partReader) Read(d []byte) (n int, err error) {
	p := pr.p
	defer func() {
		p.bytesRead += n
	}()
	if p.buffer.Len() >= len(d) {
		// Internal buffer of unconsumed data is large enough for
		// the read request.  No need to parse more at the moment.
		return p.buffer.Read(d)
	}
	peek, err := p.mr.bufReader.Peek(peekBufferSize) // TODO(bradfitz): add buffer size accessor

	// Look for an immediate empty part without a leading \r\n
	// before the boundary separator.  Some MIME code makes empty
	// parts like this. Most browsers, however, write the \r\n
	// before the subsequent boundary even for empty parts and
	// won't hit this path.
	if p.bytesRead == 0 && p.mr.peekBufferIsEmptyPart(peek) {
		return 0, io.EOF
	}
	unexpectedEOF := err == io.EOF
	if err != nil && !unexpectedEOF {
		return 0, fmt.Errorf("multipart: Part Read: %v", err)
	}
	if peek == nil {
		panic("nil peek buf")
	}
	// Search the peek buffer for "\r\n--boundary". If found,
	// consume everything up to the boundary. If not, consume only
	// as much of the peek buffer as cannot hold the boundary
	// string.
	nCopy := 0
	foundBoundary := false
	if idx, isEnd := p.mr.peekBufferSeparatorIndex(peek); idx != -1 {
		nCopy = idx
		foundBoundary = isEnd
		if !isEnd && nCopy == 0 {
			nCopy = 1 // make some progress.
		}
	} else if safeCount := len(peek) - len(p.mr.nlDashBoundary); safeCount > 0 {
		nCopy = safeCount
	} else if unexpectedEOF {
		// If we've run out of peek buffer and the boundary
		// wasn't found (and can't possibly fit), we must have
		// hit the end of the file unexpectedly.
		return 0, io.ErrUnexpectedEOF
	}
	if nCopy > 0 {
		if _, err := io.CopyN(p.buffer, p.mr.bufReader, int64(nCopy)); err != nil {
			return 0, err
		}
	}
	n, err = p.buffer.Read(d)
	if err == io.EOF && !foundBoundary {
		// If the boundary hasn't been reached there's more to
		// read, so don't pass through an EOF from the buffer
		err = nil
	}
	return
}

func (p *Part) Close() error {
	io.Copy(ioutil.Discard, p)
	return nil
}

// Reader is an iterator over parts in a MIME multipart body.
// Reader's underlying parser consumes its input as needed.  Seeking
// isn't supported.
type Reader struct {
	bufReader *bufio.Reader

	currentPart *Part
	partsRead   int

	nl               []byte // "\r\n" or "\n" (set after seeing first boundary line)
	nlDashBoundary   []byte // nl + "--boundary"
	dashBoundaryDash []byte // "--boundary--"
	dashBoundary     []byte // "--boundary"
}

// NextPart returns the next part in the multipart or an error.
// When there are no more parts, the error io.EOF is returned.
func (r *Reader) GetNextPart() (*Part, error) {
	if r.currentPart != nil {
		r.currentPart.Close()
	}

	expectNewPart := false
	for {
		line, err := r.bufReader.ReadSlice('\n')

		if err == io.EOF && r.isFinalBoundary(line) {
			// If the buffer ends in "--boundary--" without the
			// trailing "\r\n", ReadSlice will return an error
			// (since it's missing the '\n'), but this is a valid
			// multipart EOF so we need to return io.EOF instead of
			// a fmt-wrapped one.
			return nil, io.EOF
		}
		if err != nil {
			return nil, fmt.Errorf("multipart: NextPart: %v", err)
		}

		if r.isBoundaryDelimiterLine(line) {
			r.partsRead++
			bp, err := newPart(r)
			if err != nil {
				return nil, err
			}
			r.currentPart = bp
			return bp, nil
		}

		if r.isFinalBoundary(line) {
			// Expected EOF
			return nil, io.EOF
		}

		if expectNewPart {
			return nil, fmt.Errorf("multipart: expecting a new Part; got line %q", string(line))
		}

		if r.partsRead == 0 {
			// skip line
			continue
		}

		// Consume the "\n" or "\r\n" separator between the
		// body of the previous part and the boundary line we
		// now expect will follow. (either a new part or the
		// end boundary)
		if bytes.Equal(line, r.nl) {
			expectNewPart = true
			continue
		}

		return nil, fmt.Errorf("multipart: unexpected line in Next(): %q", line)
	}
}

// isFinalBoundary reports whether line is the final boundary line
// indicating that all parts are over.
// It matches `^--boundary--[ \t]*(\r\n)?$`
func (mr *Reader) isFinalBoundary(line []byte) bool {
	if !bytes.HasPrefix(line, mr.dashBoundaryDash) {
		return false
	}
	rest := line[len(mr.dashBoundaryDash):]
	rest = skipLWSPChar(rest)
	return len(rest) == 0 || bytes.Equal(rest, mr.nl)
}

func (mr *Reader) isBoundaryDelimiterLine(line []byte) (ret bool) {
	// http://tools.ietf.org/html/rfc2046#section-5.1
	//   The boundary delimiter line is then defined as a line
	//   consisting entirely of two hyphen characters ("-",
	//   decimal value 45) followed by the boundary parameter
	//   value from the Content-Type header field, optional linear
	//   whitespace, and a terminating CRLF.
	if !bytes.HasPrefix(line, mr.dashBoundary) {
		return false
	}
	rest := line[len(mr.dashBoundary):]
	rest = skipLWSPChar(rest)

	// On the first part, see our lines are ending in \n instead of \r\n
	// and switch into that mode if so.  This is a violation of the spec,
	// but occurs in practice.
	if mr.partsRead == 0 && len(rest) == 1 && rest[0] == '\n' {
		mr.nl = mr.nl[1:]
		mr.nlDashBoundary = mr.nlDashBoundary[1:]
	}
	return bytes.Equal(rest, mr.nl)
}

// peekBufferIsEmptyPart reports whether the provided peek-ahead
// buffer represents an empty part. It is called only if we've not
// already read any bytes in this part and checks for the case of MIME
// software not writing the \r\n on empty parts. Some does, some
// doesn't.
//
// This checks that what follows the "--boundary" is actually the end
// ("--boundary--" with optional whitespace) or optional whitespace
// and then a newline, so we don't catch "--boundaryFAKE", in which
// case the whole line is part of the data.
func (mr *Reader) peekBufferIsEmptyPart(peek []byte) bool {
	// End of parts case.
	// Test whether peek matches `^--boundary--[ \t]*(?:\r\n|$)`
	if bytes.HasPrefix(peek, mr.dashBoundaryDash) {
		rest := peek[len(mr.dashBoundaryDash):]
		rest = skipLWSPChar(rest)
		return bytes.HasPrefix(rest, mr.nl) || len(rest) == 0
	}
	if !bytes.HasPrefix(peek, mr.dashBoundary) {
		return false
	}
	// Test whether rest matches `^[ \t]*\r\n`)
	rest := peek[len(mr.dashBoundary):]
	rest = skipLWSPChar(rest)
	return bytes.HasPrefix(rest, mr.nl)
}

// peekBufferSeparatorIndex returns the index of mr.nlDashBoundary in
// peek and whether it is a real boundary (and not a prefix of an
// unrelated separator). To be the end, the peek buffer must contain a
// newline after the boundary or contain the ending boundary (--separator--).
func (mr *Reader) peekBufferSeparatorIndex(peek []byte) (idx int, isEnd bool) {
	idx = bytes.Index(peek, mr.nlDashBoundary)
	if idx == -1 {
		return
	}

	peek = peek[idx+len(mr.nlDashBoundary):]
	if len(peek) == 0 || len(peek) == 1 && peek[0] == '-' {
		return idx, false
	}
	if len(peek) > 1 && peek[0] == '-' && peek[1] == '-' {
		return idx, true
	}
	peek = skipLWSPChar(peek)
	// Don't have a complete line after the peek.
	if bytes.IndexByte(peek, '\n') == -1 {
		return idx, false
	}
	if len(peek) > 0 && peek[0] == '\n' {
		return idx, true
	}
	if len(peek) > 1 && peek[0] == '\r' && peek[1] == '\n' {
		return idx, true
	}
	return idx, false
}

// skipLWSPChar returns b with leading spaces and tabs removed.
// RFC 822 defines:
//    LWSP-char = SPACE / HTAB
func skipLWSPChar(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t') {
		b = b[1:]
	}
	return b
}

// MultipartReader returns a MIME multipart reader if this is a
// multipart/form-data POST request, else returns nil and an error.
// Use this function instead of ParseMultipartForm to
// process the request body as a stream.
func GetMultipartReader(r *http.Request) (*Reader, error) {
	if r.MultipartForm == multipartByReader {
		return nil, errors.New("http: MultipartReader called twice")
	}
	if r.MultipartForm != nil {
		return nil, errors.New("http: multipart handled by ParseMultipartForm")
	}
	r.MultipartForm = multipartByReader
	return getMultipartReader(r)
}

func getMultipartReader(r *http.Request) (*Reader, error) {
	v := r.Header.Get("Content-Type")
	if v == "" {
		return nil, http.ErrNotMultipart
	}
	d, params, err := ParseMediaType(v)
	if err != nil || d != "multipart/form-data" {
		return nil, http.ErrNotMultipart
	}
	boundary, ok := params["boundary"]
	if !ok {
		return nil, http.ErrMissingBoundary
	}
	return NewReader(r.Body, boundary), nil
}

// ParseMediaType parses a media type value and any optional
// parameters, per RFC 1521.  Media types are the values in
// Content-Type and Content-Disposition headers (RFC 2183).
// On success, ParseMediaType returns the media type converted
// to lowercase and trimmed of white space and a non-nil map.
// The returned map, params, maps from the lowercase
// attribute to the attribute value with its case preserved.
func ParseMediaType(v string) (mediatype string, params map[string]string, err error) {
	i := strings.Index(v, ";")
	if i == -1 {
		i = len(v)
	}
	mediatype = strings.TrimSpace(strings.ToLower(v[0:i]))

	err = checkMediaTypeDisposition(mediatype)
	if err != nil {
		return "", nil, err
	}

	params = make(map[string]string)

	// Map of base parameter name -> parameter name -> value
	// for parameters containing a '*' character.
	// Lazily initialized.
	var continuation map[string]map[string]string

	v = v[i:]
	for len(v) > 0 {
		v = strings.TrimLeftFunc(v, unicode.IsSpace)
		if len(v) == 0 {
			break
		}
		key, value, rest := consumeMediaParam(v)
		if key == "" {
			if strings.TrimSpace(rest) == ";" {
				// Ignore trailing semicolons.
				// Not an error.
				return
			}
			// Parse error.
			return "", nil, errors.New("mime: invalid media parameter")
		}

		pmap := params
		if idx := strings.Index(key, "*"); idx != -1 {
			baseName := key[:idx]
			if continuation == nil {
				continuation = make(map[string]map[string]string)
			}
			var ok bool
			if pmap, ok = continuation[baseName]; !ok {
				continuation[baseName] = make(map[string]string)
				pmap = continuation[baseName]
			}
		}
		if _, exists := pmap[key]; exists {
			// Duplicate parameter name is bogus.
			return "", nil, errors.New("mime: duplicate parameter name")
		}
		pmap[key] = value
		v = rest
	}

	// Stitch together any continuations or things with stars
	// (i.e. RFC 2231 things with stars: "foo*0" or "foo*")
	var buf bytes.Buffer
	for key, pieceMap := range continuation {
		singlePartKey := key + "*"
		if v, ok := pieceMap[singlePartKey]; ok {
			decv := decode2231Enc(v)
			params[key] = decv
			continue
		}

		buf.Reset()
		valid := false
		for n := 0; ; n++ {
			simplePart := fmt.Sprintf("%s*%d", key, n)
			if v, ok := pieceMap[simplePart]; ok {
				valid = true
				buf.WriteString(v)
				continue
			}
			encodedPart := simplePart + "*"
			if v, ok := pieceMap[encodedPart]; ok {
				valid = true
				if n == 0 {
					buf.WriteString(decode2231Enc(v))
				} else {
					decv, _ := percentHexUnescape(v)
					buf.WriteString(decv)
				}
			} else {
				break
			}
		}
		if valid {
			params[key] = buf.String()
		}
	}

	return
}

func consumeMediaParam(v string) (param, value, rest string) {
	rest = strings.TrimLeftFunc(v, unicode.IsSpace)
	if !strings.HasPrefix(rest, ";") {
		return "", "", v
	}
	rest = rest[1:] // consume semicolon
	rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
	param, rest = consumeToken(rest)
	param = strings.ToLower(param)
	if param == "" {
		return "", "", v
	}
	rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
	if !strings.HasPrefix(rest, "=") {
		return "", "", v
	}
	rest = rest[1:] // consume equals sign
	rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
	value, rest2 := consumeValue(rest)
	if value == "" && rest2 == rest {
		return "", "", v
	}
	rest = rest2
	return param, value, rest
}

// consumeToken consumes a token from the beginning of provided
// string, per RFC 2045 section 5.1 (referenced from 2183), and return
// the token consumed and the rest of the string.  Returns ("", v) on
// failure to consume at least one character.
func consumeToken(v string) (token, rest string) {
	notPos := strings.IndexFunc(v, isNotTokenChar)
	if notPos == -1 {
		return v, ""
	}
	if notPos == 0 {
		return "", v
	}
	return v[0:notPos], v[notPos:]
}

// consumeValue consumes a "value" per RFC 2045, where a value is
// either a 'token' or a 'quoted-string'.  On success, consumeValue
// returns the value consumed (and de-quoted/escaped, if a
// quoted-string) and the rest of the string.  On failure, returns
// ("", v).
func consumeValue(v string) (value, rest string) {
	if v == "" {
		return
	}
	if v[0] != '"' {
		//return consumeToken(v)
		return v, ""
	}

	// parse a quoted-string
	rest = v[1:] // consume the leading quote
	buffer := new(bytes.Buffer)
	var nextIsLiteral bool
	for idx, r := range rest {
		switch {
		case nextIsLiteral:
			buffer.WriteRune(r)
			nextIsLiteral = false
		case r == '"':
			return buffer.String(), rest[idx+1:]
		case r == '\\':
			nextIsLiteral = true
		case r != '\r' && r != '\n':
			buffer.WriteRune(r)
		default:
			return "", v
		}
	}
	return "", v
}

func isNotTokenChar(r rune) bool {
	return !isTokenChar(r)
}

// isTSpecial reports whether rune is in 'tspecials' as defined by RFC
// 1521 and RFC 2045.
func isTSpecial(r rune) bool {
	return strings.IndexRune(`()<>@,;:\"/[]?=`, r) != -1
}

// isTokenChar reports whether rune is in 'token' as defined by RFC
// 1521 and RFC 2045.
func isTokenChar(r rune) bool {
	// token := 1*<any (US-ASCII) CHAR except SPACE, CTLs,
	//             or tspecials>
	return r > 0x20 && r < 0x7f && !isTSpecial(r)
}

func checkMediaTypeDisposition(s string) error {
	typ, rest := consumeToken(s)
	if typ == "" {
		return errors.New("mime: no media type")
	}
	if rest == "" {
		return nil
	}
	if !strings.HasPrefix(rest, "/") {
		return errors.New("mime: expected slash after first token")
	}
	subtype, rest := consumeToken(rest[1:])
	if subtype == "" {
		return errors.New("mime: expected token after slash")
	}
	if rest != "" {
		return errors.New("mime: unexpected content after media subtype")
	}
	return nil
}

func decode2231Enc(v string) string {
	sv := strings.SplitN(v, "'", 3)
	if len(sv) != 3 {
		return ""
	}
	// TODO: ignoring lang in sv[1] for now. If anybody needs it we'll
	// need to decide how to expose it in the API. But I'm not sure
	// anybody uses it in practice.
	charset := strings.ToLower(sv[0])
	if charset != "us-ascii" && charset != "utf-8" {
		// TODO: unsupported encoding
		return ""
	}
	encv, _ := percentHexUnescape(sv[2])
	return encv
}

func percentHexUnescape(s string) (string, error) {
	// Count %, check that they're well-formed.
	percents := 0
	for i := 0; i < len(s); {
		if s[i] != '%' {
			i++
			continue
		}
		percents++
		if i+2 >= len(s) || !ishex(s[i+1]) || !ishex(s[i+2]) {
			s = s[i:]
			if len(s) > 3 {
				s = s[0:3]
			}
			return "", fmt.Errorf("mime: bogus characters after %%: %q", s)
		}
		i += 3
	}
	if percents == 0 {
		return s, nil
	}

	t := make([]byte, len(s)-2*percents)
	j := 0
	for i := 0; i < len(s); {
		switch s[i] {
		case '%':
			t[j] = unhex(s[i+1])<<4 | unhex(s[i+2])
			j++
			i += 3
		default:
			t[j] = s[i]
			j++
			i++
		}
	}
	return string(t), nil
}

func ishex(c byte) bool {
	switch {
	case '0' <= c && c <= '9':
		return true
	case 'a' <= c && c <= 'f':
		return true
	case 'A' <= c && c <= 'F':
		return true
	}
	return false
}

func unhex(c byte) byte {
	switch {
	case '0' <= c && c <= '9':
		return c - '0'
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10
	}
	return 0
}
