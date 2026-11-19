// Package zpl drives Zebra printers in ZPL II: every page is sent as one ^GFA graphic in Z64 form
// (zlib + base64 + CRC), status and completion are read back over the same port (~HS, ~HQES).
package zpl

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"fmt"
	"image"
	"io"
)

// encodePage writes one label: the page setup sent with every job (^PO, ^LH, ^LS, ^PW, ^LL stay in
// effect on the printer until power-off, so we never rely on what was set before) and the raster.
// The image is placed at the top left; wider than widthDots is clipped, smaller is padded white.
func encodePage(w io.Writer, img *image.Gray, widthDots, lengthDots int) error {
	rowBytes := (widthDots + 7) / 8
	raw := make([]byte, rowBytes*lengthDots)
	b := img.Bounds()
	for y := 0; y < min(lengthDots, b.Dy()); y++ {
		row := img.Pix[y*img.Stride:]
		for x := 0; x < min(widthDots, b.Dx()); x++ {
			if row[x] < 128 { // black = 1, most significant bit first
				raw[y*rowBytes+x/8] |= 0x80 >> (x % 8)
			}
		}
	}
	var z bytes.Buffer
	zw, err := zlib.NewWriterLevel(&z, zlib.BestCompression)
	if err != nil {
		return err
	}
	if _, err := zw.Write(raw); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	enc := base64.StdEncoding.EncodeToString(z.Bytes())
	_, err = fmt.Fprintf(w, "^XA^PON^LH0,0^LS0^PW%d^LL%d^FO0,0^GFA,%d,%d,%d,:Z64:%s:%04X^FS^XZ\n",
		widthDots, lengthDots, len(raw), len(raw), rowBytes, enc, crc16XMODEM([]byte(enc)))
	return err
}

// crc16XMODEM is the CRC over a Z64 payload's base64 text (polynomial 0x1021, initial 0). The ZPL
// guide doesn't name the variant; this one printed on a GK420d (2026-11-18).
func crc16XMODEM(b []byte) uint16 {
	var crc uint16
	for _, c := range b {
		crc ^= uint16(c) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
