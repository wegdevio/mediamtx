package webrtc

// HTTP-FLV fallback, for clients without WebRTC: GET /<path>.flv
//
// A normal path is served as is. An ABR path is served from one of its
// variants (no layer switching): H264 by default, highest bitrate first;
// ?codec=av1 selects the AV1 ladder (Enhanced FLV), ?start=low|<variant name>|<variant path>
// selects the layer.
//
// Muxing reuses the RTMP muxer: RTMP message bodies of audio, video and
// script data are FLV tag bodies.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/bytecounter"
	"github.com/bluenviron/gortmplib/pkg/message"
	"github.com/bluenviron/gortmplib/pkg/rawmessage"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/bluenviron/mediamtx/internal/auth"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/protocols/httpp"
	"github.com/bluenviron/mediamtx/internal/protocols/rtmp"
	"github.com/bluenviron/mediamtx/internal/stream"
)

// flvConn implements gortmplib.Conn. Messages are marshaled into RTMP chunks
// and read back as raw messages, whose bodies are written as FLV tags.
type flvConn struct {
	w   http.ResponseWriter
	ctx context.Context

	buf      bytes.Buffer
	mw       *message.Writer
	rr       *rawmessage.Reader
	sent     uint64
	tsOffset time.Duration
	tsSet    bool
}

func (c *flvConn) initialize() error {
	c.mw = &message.Writer{BW: &c.buf, BCW: bytecounter.NewWriter(&c.buf)}
	c.mw.Initialize()
	c.rr = &rawmessage.Reader{BR: &c.buf, BCR: bytecounter.NewReader(&c.buf)}
	c.rr.Initialize()

	// larger chunks, less overhead in the internal round trip
	err := c.mw.Write(&message.SetChunkSize{Value: 65536})
	if err != nil {
		return err
	}
	_, err = c.rr.Read()
	if err != nil {
		return err
	}
	err = c.rr.SetChunkSize(65536)
	if err != nil {
		return err
	}

	// header: version 1, audio + video, header size 9, PreviousTagSize0
	return c.writeRaw([]byte{'F', 'L', 'V', 1, 0x05, 0, 0, 0, 9, 0, 0, 0, 0})
}

func (c *flvConn) writeRaw(b []byte) error {
	n, err := c.w.Write(b)
	c.sent += uint64(n)
	return err
}

// BytesReceived implements gortmplib.Conn.
func (c *flvConn) BytesReceived() uint64 {
	return 0
}

// BytesSent implements gortmplib.Conn.
func (c *flvConn) BytesSent() uint64 {
	return c.sent
}

// Read implements gortmplib.Conn. Nothing is received from a HTTP client.
func (c *flvConn) Read() (message.Message, error) {
	<-c.ctx.Done()
	return nil, fmt.Errorf("terminated")
}

// Write implements gortmplib.Conn.
func (c *flvConn) Write(msg message.Message) error {
	// FLV files carry onMetaData without the RTMP '@setDataFrame' wrapper
	if d, ok := msg.(*message.DataAMF0); ok && len(d.Payload) != 0 && d.Payload[0] == "@setDataFrame" {
		d2 := *d
		d2.Payload = d.Payload[1:]
		msg = &d2
	}

	err := c.mw.Write(msg)
	if err != nil {
		return err
	}
	raw, err := c.rr.Read()
	if err != nil {
		return err
	}

	switch message.Type(raw.Type) {
	case message.TypeAudio, message.TypeVideo, message.TypeDataAMF0:
	default:
		return nil
	}

	// start the timeline at zero for every client
	ts := raw.Timestamp
	if raw.Type != uint8(message.TypeDataAMF0) {
		if !c.tsSet {
			c.tsOffset = ts
			c.tsSet = true
		}
		ts = max(ts-c.tsOffset, 0)
	} else {
		ts = 0
	}
	ms := uint32(ts / time.Millisecond)

	tag := make([]byte, 11+len(raw.Body)+4)
	tag[0] = raw.Type
	tag[1] = byte(len(raw.Body) >> 16)
	tag[2] = byte(len(raw.Body) >> 8)
	tag[3] = byte(len(raw.Body))
	tag[4] = byte(ms >> 16)
	tag[5] = byte(ms >> 8)
	tag[6] = byte(ms)
	tag[7] = byte(ms >> 24)
	copy(tag[11:], raw.Body)
	binary.BigEndian.PutUint32(tag[11+len(raw.Body):], uint32(11+len(raw.Body)))

	err = c.writeRaw(tag)
	if err != nil {
		return err
	}

	if f, ok := c.w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// flvNetConn satisfies the net.Conn that rtmp.FromStream uses for write deadlines.
// Deadlines are already applied by the HTTP server before every write.
type flvNetConn struct {
	net.Conn
}

func (flvNetConn) SetWriteDeadline(time.Time) error {
	return nil
}

// flvReader is the defs.Reader of a HTTP-FLV request.
type flvReader struct {
	s      *httpServer
	uuid   uuid.UUID
	remote string
	closed chan struct{}
}

// Log implements logger.Writer.
func (r *flvReader) Log(level logger.Level, format string, args ...any) {
	r.s.Log(level, "[flv %s] "+format, append([]any{r.remote}, args...)...)
}

// Close implements defs.Reader.
func (r *flvReader) Close() {
	select {
	case <-r.closed:
	default:
		close(r.closed)
	}
}

// APIReaderDescribe implements defs.Reader.
func (r *flvReader) APIReaderDescribe() *defs.APIPathReader {
	return &defs.APIPathReader{
		Type: defs.APIPathReaderTypeWebRTCSession,
		ID:   r.uuid.String(),
	}
}

// flvPickVariant picks the variant of an ABR path served by HTTP-FLV.
func flvPickVariant(pathConf *conf.Path, pathName string, query string) (string, string, error) {
	q, _ := url.ParseQuery(query)

	codec := q.Get("codec")
	if codec == "" {
		codec = "h264"
		if !slices.ContainsFunc(pathConf.ABRVariants, func(v conf.ABRVariant) bool { return v.Codec == "h264" }) {
			codec = pathConf.ABRVariants[0].Codec
		}
	}

	var matches []string
	if pathConf.Regexp != nil {
		matches = pathConf.Regexp.FindStringSubmatch(pathName)
	}

	var variants []conf.ABRVariant
	for _, v := range pathConf.ABRVariants {
		if v.Codec == codec {
			v.Path = abrResolvePath(v.Path, matches)
			variants = append(variants, v)
		}
	}
	if len(variants) == 0 {
		return "", "", fmt.Errorf("no %s variant", codec)
	}
	slices.SortStableFunc(variants, func(x, y conf.ABRVariant) int { return y.Bitrate - x.Bitrate })

	pick := variants[0]
	switch h := q.Get("start"); h {
	case "", "high":
	case "low":
		pick = variants[len(variants)-1]
	default:
		for _, v := range variants {
			if v.Name == h || v.Path == h {
				pick = v
			}
		}
	}

	return pick.Path, codec, nil
}

func (s *httpServer) onFLV(ctx *gin.Context, pathName string) {
	creds := &auth.Credentials{}
	creds.FromHTTP(ctx.Request)

	fr := &flvReader{
		s:      s,
		uuid:   uuid.New(),
		remote: httpp.RemoteAddr(ctx),
		closed: make(chan struct{}),
	}

	accessReq := defs.PathAccessRequest{
		Name:                 pathName,
		Query:                ctx.Request.URL.RawQuery,
		UserAgent:            ctx.Request.Header.Get("User-Agent"),
		Proto:                auth.ProtocolWebRTC,
		ID:                   &fr.uuid,
		Credentials:          creds,
		IP:                   net.ParseIP(ctx.ClientIP()),
		EnableAskCredentials: true,
	}

	confRes, err := s.pathManager.FindPathConf(defs.PathFindPathConfReq{
		Author:        fr,
		AccessRequest: accessReq,
	})
	if err != nil {
		s.writeFLVError(ctx, err)
		return
	}

	// ABR paths are virtual: serve one of their variants
	var fourCcList amf0.StrictArray
	if len(confRes.Conf.ABRVariants) != 0 {
		var variant, codec string
		variant, codec, err = flvPickVariant(confRes.Conf, pathName, ctx.Request.URL.RawQuery)
		if err != nil {
			s.writeErrorNoLog(ctx, http.StatusNotAcceptable, err)
			return
		}
		if codec == "av1" {
			fourCcList = amf0.StrictArray{"av01"}
		}
		accessReq.Name = variant
		accessReq.SkipAuth = true // authenticated on the ABR path
	}

	res, err := s.pathManager.AddReader(defs.PathAddReaderReq{
		Author:        fr,
		AccessRequest: accessReq,
	})
	if err != nil {
		s.writeFLVError(ctx, err)
		return
	}
	defer res.Path.RemoveReader(defs.PathRemoveReaderReq{Author: fr})

	reqCtx, cancel := context.WithCancel(ctx.Request.Context())
	defer cancel()

	ctx.Header("Content-Type", "video/x-flv")
	ctx.Header("Cache-Control", "no-cache")
	ctx.Writer.WriteHeader(http.StatusOK)

	c := &flvConn{w: ctx.Writer, ctx: reqCtx}
	err = c.initialize()
	if err != nil {
		return
	}

	r := &stream.Reader{Parent: fr}

	_, err = rtmp.FromStream(
		res.Stream.OrigDesc,
		res.Stream.OutDescCopy(),
		r,
		c,
		flvNetConn{},
		time.Duration(s.writeTimeout),
		fourCcList)
	if err != nil {
		fr.Log(logger.Warn, "%v", err)
		return
	}

	fr.Log(logger.Info, "is reading from path '%s' via HTTP-FLV, %s",
		res.Path.Name(), defs.FormatsInfo(r.Formats()))

	res.Stream.AddReader(r)
	defer res.Stream.RemoveReader(r)

	select {
	case err = <-r.Error():
		fr.Log(logger.Info, "closed: %v", err)
	case <-fr.closed:
		fr.Log(logger.Info, "closed: path closed the reader")
	case <-reqCtx.Done():
		fr.Log(logger.Info, "closed: client went away")
	}
}

func (s *httpServer) writeFLVError(ctx *gin.Context, err error) {
	if terr, ok := errors.AsType[*auth.Error](err); ok {
		if terr.AskCredentials {
			ctx.Header("WWW-Authenticate", `Basic realm="mediamtx"`)
		}
		s.writeErrorNoLog(ctx, http.StatusUnauthorized, fmt.Errorf("authentication error"))
		return
	}
	if _, ok := errors.AsType[*defs.PathNoStreamAvailableError](err); ok {
		s.writeErrorNoLog(ctx, http.StatusNotFound, err)
		return
	}
	s.writeErrorNoLog(ctx, http.StatusBadRequest, err)
}
