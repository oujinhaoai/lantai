package assettools

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestBoundedImageActualPixelsAndInvalidInputs(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 640, 320))
	for y := range 320 {
		for x := range 640 {
			source.SetNRGBA(x, y, color.NRGBA{R: 200, A: 128})
		}
	}
	for _, format := range []string{"png", "jpeg"} {
		t.Run(format, func(t *testing.T) {
			var raw bytes.Buffer
			var err error
			if format == "png" {
				err = png.Encode(&raw, source)
			} else {
				err = jpeg.Encode(&raw, source, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			output, info, err := ImageProxy(t.Context(), raw.Bytes(), 320)
			if err != nil || info.Width != 320 || info.Height != 160 {
				t.Fatal(info, err)
			}
			actual, err := png.Decode(bytes.NewReader(output))
			if err != nil || actual.Bounds().Dx() != 320 || actual.Bounds().Dy() != 160 {
				t.Fatal("output bytes not actual PNG", err)
			}
			if format == "png" {
				_, _, _, a := actual.At(0, 0).RGBA()
				if a != 128*257 {
					t.Fatal("transparency lost", a)
				}
			}
		})
	}
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, source); err != nil {
		t.Fatal(err)
	}
	huge := append([]byte(nil), pngBytes.Bytes()...)
	binary.BigEndian.PutUint32(huge[16:20], 16385)
	binary.BigEndian.PutUint32(huge[29:33], crc32.ChecksumIEEE(huge[12:29]))
	pixels := append([]byte(nil), pngBytes.Bytes()...)
	binary.BigEndian.PutUint32(pixels[16:20], 8192)
	binary.BigEndian.PutUint32(pixels[20:24], 8192)
	binary.BigEndian.PutUint32(pixels[29:33], crc32.ChecksumIEEE(pixels[12:29]))
	if _, _, err := ImageProxy(t.Context(), pixels, 320); err == nil || !strings.Contains(err.Error(), "pixel budget") {
		t.Fatal("pixel budget not checked before allocation", err)
	}
	animation := append([]byte(nil), pngBytes.Bytes()[:33]...)
	animation = append(animation, []byte{0, 0, 0, 0, 'a', 'c', 'T', 'L', 0, 0, 0, 0}...)
	animation = append(animation, pngBytes.Bytes()[33:]...)
	for name, raw := range map[string][]byte{"corrupt": []byte("not an image"), "huge-header": huge, "animated": animation, "bytes": make([]byte, ImageBytes+1), "truncated": pngBytes.Bytes()[:30]} {
		if _, _, err := ImageProxy(t.Context(), raw, 320); err == nil {
			t.Fatal("accepted", name)
		}
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := ImageProxy(cancelled, pngBytes.Bytes(), 320); err == nil {
		t.Fatal("ignored cancellation")
	}
	if _, _, err := ImageProxy(t.Context(), pngBytes.Bytes(), 1000000); err == nil {
		t.Fatal("unbounded requested dimension")
	}
}
func TestOBJStructureNegativeBudgetsAndNoExternalReads(t *testing.T) {
	raw := []byte("mtllib https://outside.invalid/material.mtl\nv 0 0 0\nv 1 0 0\nv 0 1 0\nf -3 -2 -1\n")
	model, err := ParseOBJ(t.Context(), raw)
	if err != nil || len(model.Edges) != 3 || model.Faces != 1 || len(model.MaterialLibraries) != 1 {
		t.Fatal(model, err)
	}
	svg, err := model.SVG(t.Context(), 320)
	if err != nil || !bytes.Contains(svg, []byte("<path")) || bytes.Contains(svg, []byte("outside")) {
		t.Fatal("SVG loaded or embedded external material", err)
	}
	for name, raw := range map[string][]byte{
		"missing": []byte("v 0 0 0\nf 1 2 3"), "zero-index": []byte("v 0 0 0\nv 1 0 0\nv 0 1 0\nf 0 1 2"), "nan": []byte("v NaN 0 0"), "curves": []byte("curv 0 1 2"), "control": []byte("v 0 0 0\x00"), "bad-utf8": {255}, "bytes": make([]byte, OBJBytes+1), "vertices": []byte(strings.Repeat("v 0 0 0\n", MaxVertices+1)), "faces": []byte("v 0 0 0\nv 1 0 0\nv 0 1 0\n" + strings.Repeat("f 1 2 3\n", MaxFaces+1)), "face-size": []byte("v 0 0 0\nf " + strings.Repeat("1 ", MaxFaceVertices+1)), "lines": []byte(strings.Repeat("\n", MaxLines)),
	} {
		if _, err := ParseOBJ(t.Context(), raw); err == nil {
			t.Fatal("accepted", name)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ParseOBJ(ctx, raw); err == nil {
		t.Fatal("OBJ ignored cancellation")
	}
	// Three disjoint cyclic edge families fill60000 edges in20000 faces.
	var lines strings.Builder
	for range MaxVertices {
		lines.WriteString("v 0 0 0\n")
	}
	for i := range MaxVertices {
		lines.WriteString("f " + strconv.Itoa(i+1) + " " + strconv.Itoa((i+127)%MaxVertices+1) + " " + strconv.Itoa((i+1207)%MaxVertices+1) + "\n")
	}
	lines.WriteString("f 1 2 3\n")
	if _, err := ParseOBJ(t.Context(), []byte(lines.String())); err == nil || !strings.Contains(err.Error(), "edge budget") {
		t.Fatal("OBJ edge budget not enforced", err)
	}
}
func TestBoundedSyntheticWebPActualDecode(t *testing.T) {
	raw, err := os.ReadFile("../../tests/fixtures/asset-derivations/synthetic-red.webp")
	if err != nil {
		t.Fatal(err)
	}
	output, info, err := ImageProxy(t.Context(), raw, 320)
	if err != nil || info.Format != "webp" || info.Width != 8 || info.Height != 4 {
		t.Fatal(info, err)
	}
	decoded, err := png.Decode(bytes.NewReader(output))
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, _ := decoded.At(0, 0).RGBA()
	if r < 60000 || g > 1000 || b > 1000 {
		t.Fatal("WebP synthetic red pixels differ", r, g, b)
	}
}
