// Package assettools contains bounded data transformations only. It never opens
// URLs, launches applications, loads plugins or reads external material paths.
package assettools

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	"image/png"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	_ "golang.org/x/image/webp"
)

const ImageBytes = 16 << 20
const ImagePixels = 16 << 20
const OBJBytes = 2 << 20
const MaxVertices = 20000
const MaxEdges = 60000
const MaxFaces = 30000
const MaxLines = 100000
const MaxFaceVertices = 64
const MaxArtifactBytes = 65 << 20

type ImageInfo struct {
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	Format       string `json:"format"`
	SourceWidth  int    `json:"source_width"`
	SourceHeight int    `json:"source_height"`
}

func validEdge(edge int) bool { return edge == 320 || edge == 512 || edge == 1024 || edge == 2048 }
func rejectAnimation(raw []byte) error {
	if len(raw) >= 8 && bytes.Equal(raw[:8], []byte{137, 80, 78, 71, 13, 10, 26, 10}) {
		for offset := 8; offset < len(raw); {
			if len(raw)-offset < 12 {
				return errors.New("truncated PNG chunk")
			}
			size := int64(binary.BigEndian.Uint32(raw[offset : offset+4]))
			if size > int64(len(raw)-offset-12) {
				return errors.New("truncated PNG chunk")
			}
			if string(raw[offset+4:offset+8]) == "acTL" {
				return errors.New("animated PNG is outside this tool")
			}
			offset += int(size) + 12
		}
	}
	if len(raw) >= 12 && string(raw[:4]) == "RIFF" && string(raw[8:12]) == "WEBP" {
		if int64(binary.LittleEndian.Uint32(raw[4:8]))+8 != int64(len(raw)) {
			return errors.New("invalid WebP container size")
		}
		for offset := 12; offset < len(raw); {
			if len(raw)-offset < 8 {
				return errors.New("truncated WebP chunk")
			}
			size := int64(binary.LittleEndian.Uint32(raw[offset+4 : offset+8]))
			if size > int64(len(raw)-offset-8) {
				return errors.New("truncated WebP chunk")
			}
			kind := string(raw[offset : offset+4])
			if kind == "ANIM" || kind == "ANMF" || kind == "VP8X" && size >= 1 && raw[offset+8]&2 != 0 {
				return errors.New("animated WebP is outside this tool")
			}
			offset += 8 + int(size) + (int(size) & 1)
		}
	}
	return nil
}
func ImageProxy(ctx context.Context, raw []byte, edge int) ([]byte, ImageInfo, error) {
	var out ImageInfo
	if err := ctx.Err(); err != nil {
		return nil, out, err
	}
	if len(raw) == 0 || len(raw) > ImageBytes || !validEdge(edge) {
		return nil, out, errors.New("image byte or requested edge budget exceeded")
	}
	if err := rejectAnimation(raw); err != nil {
		return nil, out, err
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, out, err
	}
	if format != "png" && format != "jpeg" && format != "webp" {
		return nil, out, errors.New("only PNG, JPEG and WebP are supported")
	}
	if config.Width < 1 || config.Height < 1 || config.Width > 16384 || config.Height > 16384 || int64(config.Width)*int64(config.Height) > ImagePixels {
		return nil, out, errors.New("image dimension or pixel budget exceeded")
	}
	source, decodedFormat, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, out, err
	}
	if decodedFormat != format || source.Bounds().Dx() != config.Width || source.Bounds().Dy() != config.Height {
		return nil, out, errors.New("decoded image differs from bounded header")
	}
	factor := math.Min(1, float64(edge)/float64(max(config.Width, config.Height)))
	width, height := max(1, int(math.Floor(float64(config.Width)*factor+0.5))), max(1, int(math.Floor(float64(config.Height)*factor+0.5)))
	target := image.NewNRGBA(image.Rect(0, 0, width, height))
	bounds := source.Bounds()
	// Deterministic nearest-neighbor sampling; check cancellation on each row.
	// No EXIF transform or general media transcoder is implied.
	for y := 0; y < height; y++ {
		if err := ctx.Err(); err != nil {
			return nil, out, err
		}
		sy := bounds.Min.Y + min(config.Height-1, int((float64(y)+0.5)*float64(config.Height)/float64(height)))
		for x := 0; x < width; x++ {
			sx := bounds.Min.X + min(config.Width-1, int((float64(x)+0.5)*float64(config.Width)/float64(width)))
			target.Set(x, y, source.At(sx, sy))
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, target); err != nil {
		return nil, out, err
	}
	if err := ctx.Err(); err != nil {
		return nil, out, err
	}
	out = ImageInfo{Width: width, Height: height, Format: format, SourceWidth: config.Width, SourceHeight: config.Height}
	return buffer.Bytes(), out, nil
}

type Wireframe struct {
	Vertices                   [][3]float64 `json:"vertices"`
	Edges                      [][2]int     `json:"edges"`
	Faces                      int          `json:"faces"`
	MaterialLibraries          []string     `json:"materialLibraries"`
	MaterialLibrariesTruncated bool         `json:"material_libraries_truncated,omitempty"`
}

var faceIndex = regexp.MustCompile(`^-?[0-9]+(?:/(?:-?[0-9]+)?(?:/-?[0-9]+)?)?$`)

var objDecimal = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

// Match finite ECMAScript Number syntax used by the page, including integer
// radix prefixes but excluding Go-only hexadecimal floating literals.
func objNumber(field string) (float64, error) {
	if objDecimal.MatchString(field) {
		return strconv.ParseFloat(field, 64)
	}
	if len(field) > 2 && field[0] == '0' {
		base := 0
		switch field[1] {
		case 'x', 'X':
			base = 16
		case 'b', 'B':
			base = 2
		case 'o', 'O':
			base = 8
		}
		if base != 0 {
			value, err := strconv.ParseUint(field[2:], base, 64)
			return float64(value), err
		}
	}
	return 0, errors.New("invalid OBJ numeric syntax")
}
func objWhitespace(r rune) bool { return r == '\ufeff' || r != '\u0085' && unicode.IsSpace(r) }
func ParseOBJ(ctx context.Context, raw []byte) (Wireframe, error) {
	out := Wireframe{Vertices: [][3]float64{}, Edges: [][2]int{}, MaterialLibraries: []string{}}
	if len(raw) > OBJBytes || !utf8.Valid(raw) {
		return out, errors.New("OBJ byte/UTF8 budget exceeded")
	}
	for _, r := range string(raw) {
		if r < 32 && r != '\t' && r != '\r' && r != '\n' || r == 127 {
			return out, errors.New("OBJ contains control characters")
		}
	}
	lines := strings.Split(string(raw), "\n")
	if len(lines) > MaxLines {
		return out, errors.New("OBJ line budget exceeded")
	}
	seen := map[[2]int]bool{}
	for _, line := range lines {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		line, _, _ = strings.Cut(line, "#")
		tokens := strings.FieldsFunc(line, objWhitespace)
		if len(tokens) == 0 {
			continue
		}
		kind, fields := tokens[0], tokens[1:]
		switch kind {
		case "v":
			if len(fields) != 3 || len(out.Vertices) >= MaxVertices {
				return out, errors.New("OBJ vertex shape or count exceeds budget")
			}
			var point [3]float64
			for i, field := range fields {
				value, err := objNumber(field)
				if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) > 1e9 {
					return out, errors.New("OBJ vertex must be bounded and finite")
				}
				point[i] = value
			}
			out.Vertices = append(out.Vertices, point)
		case "f":
			if len(fields) < 3 || len(fields) > MaxFaceVertices || out.Faces >= MaxFaces {
				return out, errors.New("OBJ face budget exceeded")
			}
			out.Faces++
			indices := make([]int, len(fields))
			for i, field := range fields {
				if !faceIndex.MatchString(field) {
					return out, errors.New("OBJ face index syntax invalid")
				}
				first, _, _ := strings.Cut(field, "/")
				value, err := strconv.ParseInt(first, 10, 64)
				if err != nil || value == 0 || value > 9007199254740991 || value < -9007199254740991 {
					return out, errors.New("OBJ face index invalid")
				}
				index := value - 1
				if value < 0 {
					index = int64(len(out.Vertices)) + value
				}
				if index < 0 || index >= int64(len(out.Vertices)) {
					return out, errors.New("OBJ face references absent vertex")
				}
				indices[i] = int(index)
			}
			for i, a := range indices {
				b := indices[(i+1)%len(indices)]
				if a == b {
					continue
				}
				key := [2]int{min(a, b), max(a, b)}
				if !seen[key] {
					if len(out.Edges) >= MaxEdges {
						return out, errors.New("OBJ edge budget exceeded")
					}
					seen[key] = true
					out.Edges = append(out.Edges, [2]int{a, b})
				}
			}
		case "mtllib":
			if len(out.MaterialLibraries) >= 64 {
				out.MaterialLibrariesTruncated = true
				continue
			}
			name := strings.Join(fields, " ")
			runes := []rune(name)
			if len(runes) > 512 {
				runes = runes[:512]
				out.MaterialLibrariesTruncated = true
			}
			out.MaterialLibraries = append(out.MaterialLibraries, string(runes))
		case "curv", "surf", "l":
			return out, errors.New("OBJ curves, surfaces and lines are outside this tool")
		}
	}
	if len(out.Vertices) == 0 || len(out.Edges) == 0 {
		return out, errors.New("OBJ has no polygon wireframe")
	}
	return out, nil
}
func (w Wireframe) SVG(ctx context.Context, edge int) ([]byte, error) {
	if !validEdge(edge) {
		return nil, errors.New("invalid wireframe edge")
	}
	points := make([][2]float64, len(w.Vertices))
	left, right, top, bottom := math.Inf(1), math.Inf(-1), math.Inf(1), math.Inf(-1)
	for i, v := range w.Vertices {
		p := [2]float64{v[0] - v[2]*0.5, -v[1] + (v[0]+v[2])*0.25}
		points[i] = p
		left = math.Min(left, p[0])
		right = math.Max(right, p[0])
		top = math.Min(top, p[1])
		bottom = math.Max(bottom, p[1])
	}
	scale := float64(edge-16) / math.Max(math.Max(right-left, bottom-top), 1e-12)
	var buffer bytes.Buffer
	fmt.Fprintf(&buffer, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d"><g fill="none" stroke="black" stroke-width="1">`, edge, edge)
	for _, pair := range w.Edges {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		a, b := points[pair[0]], points[pair[1]]
		fmt.Fprintf(&buffer, `<path d="M%.3f %.3f L%.3f %.3f"/>`, 8+(a[0]-left)*scale, 8+(a[1]-top)*scale, 8+(b[0]-left)*scale, 8+(b[1]-top)*scale)
	}
	buffer.WriteString("</g></svg>\n")
	if buffer.Len() > 8<<20 {
		return nil, errors.New("wireframe output budget exceeded")
	}
	return buffer.Bytes(), nil
}
func (w Wireframe) JSON() ([]byte, error) {
	return json.Marshal(struct {
		Schema string `json:"schema"`
		Wireframe
	}{Schema: "lantai.obj-structure/v1", Wireframe: w})
}
