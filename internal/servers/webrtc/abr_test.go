package webrtc

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
)

const abrTestOffer = "v=0\r\n" +
	"o=- 1 1 IN IP4 127.0.0.1\r\n" +
	"s=-\r\n" +
	"t=0 0\r\n" +
	"m=video 9 UDP/TLS/RTP/SAVPF 96 102 45\r\n" +
	"a=rtpmap:96 VP8/90000\r\n" +
	"a=rtpmap:102 H264/90000\r\n" +
	"a=rtpmap:45 AV1/90000\r\n" +
	"m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
	"a=rtpmap:111 opus/48000/2\r\n"

func TestABRPickCodec(t *testing.T) {
	both := []conf.ABRVariant{
		{Path: "a", Codec: "h264", Bitrate: 1},
		{Path: "b", Codec: "av1", Bitrate: 1},
	}

	require.Equal(t, []string{"h264", "av1"}, offerCodecs([]byte(abrTestOffer)))

	c, err := abrPickCodec([]byte(abrTestOffer), "", both)
	require.NoError(t, err)
	require.Equal(t, "av1", c) // server preference

	c, err = abrPickCodec([]byte(abrTestOffer), "codec=h264", both)
	require.NoError(t, err)
	require.Equal(t, "h264", c)

	c, err = abrPickCodec([]byte(abrTestOffer), "", both[:1])
	require.NoError(t, err)
	require.Equal(t, "h264", c)

	noAV1 := "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\nm=video 9 UDP/TLS/RTP/SAVPF 96\r\na=rtpmap:96 VP8/90000\r\n"
	_, err = abrPickCodec([]byte(noAV1), "", both)
	require.Error(t, err)
}

func TestABRResolvePath(t *testing.T) {
	m := regexp.MustCompile(`^abr/(.+)_(\d+)$`).FindStringSubmatch("abr/stream_12")
	require.Equal(t, "hbn/stream_h264_1080", abrResolvePath("hbn/$G1_h264_1080", m))
	require.Equal(t, "x/12/stream", abrResolvePath("x/$G2/$G1", m))
	require.Equal(t, "static", abrResolvePath("static", nil))
}
