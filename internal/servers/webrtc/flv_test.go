package webrtc

import (
	"bytes"
	"context"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/bluenviron/gortmplib/pkg/message"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
)

func TestFLVPickVariant(t *testing.T) {
	pc := &conf.Path{
		Regexp: regexp.MustCompile(`^live/(.+)$`),
		ABRVariants: []conf.ABRVariant{
			{Path: "hbn/$G1_av1", Codec: "av1", Name: "1080p", Bitrate: 3000000},
			{Path: "hbn/$G1_720", Codec: "h264", Name: "720p", Bitrate: 1500000},
			{Path: "hbn/$G1_1080", Codec: "h264", Name: "1080p", Bitrate: 4500000},
		},
	}

	p, c, err := flvPickVariant(pc, "live/s", "")
	require.NoError(t, err)
	require.Equal(t, "hbn/s_1080", p)
	require.Equal(t, "h264", c)

	p, _, err = flvPickVariant(pc, "live/s", "start=low")
	require.NoError(t, err)
	require.Equal(t, "hbn/s_720", p)

	p, _, err = flvPickVariant(pc, "live/s", "start=720p")
	require.NoError(t, err)
	require.Equal(t, "hbn/s_720", p)

	p, c, err = flvPickVariant(pc, "live/s", "codec=av1")
	require.NoError(t, err)
	require.Equal(t, "hbn/s_av1", p)
	require.Equal(t, "av1", c)

	_, _, err = flvPickVariant(pc, "live/s", "codec=vp9")
	require.Error(t, err)
}

func TestFLVConn(t *testing.T) {
	w := httptest.NewRecorder()
	c := &flvConn{w: w, ctx: context.Background()}
	require.NoError(t, c.initialize())

	require.NoError(t, c.Write(&message.DataAMF0{
		ChunkStreamID:   4,
		MessageStreamID: 0x1000000,
		Payload:         []any{"@setDataFrame", "onMetaData", "x"},
	}))
	require.NoError(t, c.Write(&message.Audio{
		ChunkStreamID:   4,
		MessageStreamID: 0x1000000,
		Codec:           message.CodecMPEG4Audio,
		Rate:            message.AudioRate44100,
		Depth:           message.AudioDepth16,
		IsStereo:        true,
		AACType:         message.AudioAACTypeAU,
		AU:              []byte{1, 2, 3},
		DTS:             5000_000_000,
	}))

	b := w.Body.Bytes()
	require.Equal(t, []byte{'F', 'L', 'V', 1, 0x05, 0, 0, 0, 9, 0, 0, 0, 0}, b[:13])
	b = b[13:]

	// script tag without @setDataFrame
	require.Equal(t, byte(18), b[0])
	n := int(b[1])<<16 | int(b[2])<<8 | int(b[3])
	require.False(t, bytes.Contains(b[11:11+n], []byte("@setDataFrame")))
	b = b[11+n+4:]

	// audio tag, timeline starting at zero
	require.Equal(t, byte(8), b[0])
	require.Equal(t, []byte{0, 0, 0, 0}, b[4:8])
	require.Equal(t, []byte{0xAF, 1, 1, 2, 3}, b[11:16])
}
