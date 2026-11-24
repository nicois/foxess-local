// Package label makes an OpenStick appliance's label (design D-031):
// the hotspot's name and password, a Wi-Fi QR code that joins it, and
// the wizard's .local address. Scanning the code is the whole first
// step of setting the stick up.
package label

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"strconv"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
	"rsc.io/qr"

	"foxesslocal/internal/wizard/identity"
	"foxesslocal/internal/wizard/web"
)

// Layout, in pixels unless noted.
const (
	qrLevel   = qr.M // 15% of the code may be smudged
	module    = 12   // one QR module
	quiet     = 4    // the QR quiet zone, in modules
	margin    = 24
	textScale = 3 // basicfont's 7x13 glyphs, enlarged
	lineGap   = 12
	midGrey   = 0x80 // a font pixel darker than this is ink
)

// WiFi is the QR payload that joins a WPA network, with the format's
// special characters (\ ; , : ") escaped.
func WiFi(ssid, psk string) string {
	esc := strings.NewReplacer(`\`, `\\`, `;`, `\;`, `,`, `\,`, `:`, `\:`, `"`, `\"`)
	return "WIFI:T:WPA;S:" + esc.Replace(ssid) + ";P:" + esc.Replace(psk) + ";;"
}

// URL is the wizard's address on the home network.
func URL(id identity.Identity) string {
	return "http://" + id.Host + ".local:" + strconv.Itoa(web.Port)
}

// Text is the label as text, for the preparer.
func Text(id identity.Identity) string {
	return fmt.Sprintf("foxess OpenStick %s\n"+
		"Set up: scan the QR code, or join the Wi-Fi network\n"+
		"  Network:  %s\n"+
		"  Password: %s\n"+
		"Later, on your home network: %s\n", id.Host, id.SSID, id.Password, URL(id))
}

// lines are the PNG's text, under the code.
func lines(id identity.Identity) []string {
	return []string{"Wi-Fi  " + id.SSID, "Password  " + id.Password, strings.TrimPrefix(URL(id), "http://")}
}

// PNG is the printable label: the QR code, then the text.
func PNG(id identity.Identity) ([]byte, error) {
	code, err := qr.Encode(WiFi(id.SSID, id.Password), qrLevel)
	if err != nil {
		return nil, fmt.Errorf("label: %w", err)
	}
	face := basicfont.Face7x13
	text := lines(id)
	glyphW, lineH := face.Advance*textScale, face.Height*textScale
	qrW := (code.Size + 2*quiet) * module
	w := qrW
	for _, l := range text {
		w = max(w, len(l)*glyphW)
	}
	w += 2 * margin
	h := 2*margin + qrW + len(text)*(lineH+lineGap)
	img := image.NewGray(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.White, image.Point{}, draw.Src)
	origin := margin + quiet*module
	for y := range code.Size {
		for x := range code.Size {
			if code.Black(x, y) {
				r := image.Rect(origin+x*module, origin+y*module, origin+(x+1)*module, origin+(y+1)*module)
				draw.Draw(img, r, image.Black, image.Point{}, draw.Src)
			}
		}
	}
	top := margin + qrW
	for _, l := range text {
		drawText(img, face, l, margin, top)
		top += lineH + lineGap
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		return nil, fmt.Errorf("label: %w", err)
	}
	return b.Bytes(), nil
}

// drawText draws s with its top-left corner at (x, y), each font pixel
// enlarged to textScale×textScale (basicfont is small for print).
func drawText(dst *image.Gray, face *basicfont.Face, s string, x, y int) {
	small := image.NewGray(image.Rect(0, 0, len(s)*face.Advance, face.Height))
	draw.Draw(small, small.Bounds(), image.White, image.Point{}, draw.Src)
	d := font.Drawer{Dst: small, Src: image.Black, Face: face, Dot: fixed.P(0, face.Ascent)}
	d.DrawString(s)
	b := small.Bounds()
	for sy := b.Min.Y; sy < b.Max.Y; sy++ {
		for sx := b.Min.X; sx < b.Max.X; sx++ {
			if small.GrayAt(sx, sy).Y >= midGrey {
				continue
			}
			r := image.Rect(x+sx*textScale, y+sy*textScale, x+(sx+1)*textScale, y+(sy+1)*textScale)
			draw.Draw(dst, r, image.Black, image.Point{}, draw.Src)
		}
	}
}
