package label

import (
	"bytes"
	"image"
	"image/png"
	"strings"
	"testing"

	"rsc.io/qr"

	"foxesslocal/internal/wizard/identity"
)

const testName = "foxess-1a2b"

var testID = identity.Identity{SSID: testName, Password: "abcd-efgh-jkmn", Host: testName}

// TestWiFi: the Wi-Fi QR payload phones understand, with the format's
// special characters escaped.
func TestWiFi(t *testing.T) {
	if got := WiFi(testName, "abcd-efgh-jkmn"); got != "WIFI:T:WPA;S:"+testName+";P:abcd-efgh-jkmn;;" {
		t.Fatalf("plain: %s", got)
	}
	if got := WiFi(`a;b,c`, `p:q"r\s`); got != `WIFI:T:WPA;S:a\;b\,c;P:p\:q\"r\\s;;` {
		t.Fatalf("escaped: %s", got)
	}
}

func TestText(t *testing.T) {
	s := Text(testID)
	for _, want := range []string{"foxess-1a2b", "abcd-efgh-jkmn", "http://foxess-1a2b.local:18081"} {
		if !strings.Contains(s, want) {
			t.Errorf("label text lacks %q:\n%s", want, s)
		}
	}
}

// TestPNG: the image holds the QR code module for module (each
// module's centre pixel is dark exactly where the code is), inside a
// quiet zone, with the text below it.
func TestPNG(t *testing.T) {
	b, err := PNG(testID)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	code, err := qr.Encode(WiFi(testID.SSID, testID.Password), qrLevel)
	if err != nil {
		t.Fatal(err)
	}
	origin := margin + quiet*module
	for y := range code.Size {
		for x := range code.Size {
			if dark(img, origin+x*module+module/2, origin+y*module+module/2) != code.Black(x, y) {
				t.Fatalf("module (%d,%d) differs", x, y)
			}
		}
	}
	for i := range quiet * module {
		if dark(img, margin+i, margin+i) {
			t.Fatalf("quiet zone dark at %d", i)
		}
	}
	textTop := origin + code.Size*module + quiet*module
	if !inkBelow(img, textTop) {
		t.Fatal("no text below the code")
	}
}

func dark(img image.Image, x, y int) bool {
	r, g, b, _ := img.At(x, y).RGBA()
	return r+g+b < 3*0x8000
}

// inkBelow: some pixel at row top or below is dark.
func inkBelow(img image.Image, top int) bool {
	bd := img.Bounds()
	for y := top; y < bd.Max.Y; y++ {
		for x := bd.Min.X; x < bd.Max.X; x++ {
			if dark(img, x, y) {
				return true
			}
		}
	}
	return false
}
