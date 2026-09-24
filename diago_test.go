// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/emiago/diago/audio"
	"github.com/emiago/diago/examples"
	"github.com/emiago/diago/media"
	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/diago/testdata"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testDiagoClient(t *testing.T, onRequest func(req *sip.Request) *sip.Response, opts ...DiagoOption) *Diago {
	// Create client transaction request
	cTxReq := &clientTxRequester{
		onRequest: onRequest,
	}

	ua, _ := sipgo.NewUA()
	client, _ := sipgo.NewClient(ua)
	client.TxRequester = cTxReq
	t.Cleanup(func() {
		ua.Close()
	})

	opts = append(opts, WithClient(client))
	return NewDiago(ua, opts...)
}

func TestMain(m *testing.M) {
	examples.SetupLogger()
	m.Run()
}

func TestDiagoRegister(t *testing.T) {
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		sync.OnceFunc(func() {
			sip.NewResponseFromRequest(req, 100, "Trying", nil)
		})()

		return sip.NewResponseFromRequest(req, 200, "OK", nil)
	})

	ctx := context.TODO()
	rtx, err := dg.RegisterTransaction(ctx, sip.Uri{User: "alice", Host: "localhost"}, RegisterOptions{})
	require.NoError(t, err)

	err = rtx.Register(ctx)
	require.NoError(t, err)
}

func TestDiagoRegisterAuthorization(t *testing.T) {
	t.Skip("Do test with sending Register and authorization returned")
}

func TestDiagoInviteCallerID(t *testing.T) {

	t.Run("NoSDPInResponse", func(t *testing.T) {
		dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
			return sip.NewResponseFromRequest(req, 200, "OK", nil)
		})

		_, err := dg.Invite(context.Background(), sip.Uri{User: "alice", Host: "localhost"}, InviteOptions{})
		if assert.Error(t, err) {
			assert.Equal(t, "no SDP in response", err.Error())
		}
	})

	reqCh := make(chan *sip.Request)
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		reqCh <- req
		return sip.NewResponseFromRequest(req, 500, "", nil)
	})

	t.Run("DefaultCallerID", func(t *testing.T) {
		go dg.Invite(context.Background(), sip.Uri{User: "alice", Host: "localhost"}, InviteOptions{})
		req := <-reqCh

		assert.Equal(t, dg.ua.Name(), req.From().Address.User)
		assert.Equal(t, dg.ua.Hostname(), req.From().Address.Host)
		assert.NotEmpty(t, req.From().Params.GetOr("tag", ""))
	})

}

func TestDiagoTransportConfs(t *testing.T) {
	type testCase = struct {
		tran                    Transport
		expectedContactHostPort string
		expectedMediaHost       string
	}

	doTest := func(tc testCase) {
		tran := tc.tran
		reqCh := make(chan *sip.Request)
		dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
			reqCh <- req
			return sip.NewResponseFromRequest(req, 200, "OK", nil)
		}, WithTransport(tran))

		go dg.Invite(context.TODO(), sip.Uri{User: "alice", Host: "localhost"}, InviteOptions{})

		// Now check our req passed on client
		req := <-reqCh

		// parse SDP
		sd := sdp.SessionDescription{}
		require.NoError(t, sdp.Unmarshal(req.Body(), &sd))
		connInfo, err := sd.ConnectionInformation()
		require.NoError(t, err)

		assert.Equal(t, tc.expectedContactHostPort, req.Contact().Address.HostPort())
		assert.Equal(t, tc.expectedMediaHost, connInfo.IP.String())
	}

	t.Run("ExternalHost", func(t *testing.T) {
		tc := testCase{
			tran: Transport{
				Transport:    "udp",
				BindHost:     "127.0.0.111",
				BindPort:     15060,
				ExternalHost: "1.2.3.4",
			},
			expectedContactHostPort: "1.2.3.4:15060",
			expectedMediaHost:       "1.2.3.4",
		}

		doTest(tc)
	})

	t.Run("ExternalHostFQDN", func(t *testing.T) {
		tc := testCase{
			tran: Transport{
				Transport:    "udp",
				BindHost:     "127.0.0.111",
				BindPort:     15060,
				ExternalHost: "myhost.pbx.com",
			},
			expectedContactHostPort: "myhost.pbx.com:15060",
			expectedMediaHost:       "127.0.0.111", // Hosts are not resolved so it goes with bind
		}

		doTest(tc)
	})

	t.Run("ExternalHostFQDNExternalMedia", func(t *testing.T) {
		tc := testCase{
			tran: Transport{
				Transport:       "udp",
				BindHost:        "127.0.0.111",
				BindPort:        15060,
				ExternalHost:    "myhost.pbx.com",
				MediaExternalIP: net.IPv4(1, 2, 3, 4),
			},
			expectedContactHostPort: "myhost.pbx.com:15060",
			expectedMediaHost:       "1.2.3.4", // Hosts are not resolved so it goes with bind
		}

		doTest(tc)
	})
}

func TestDiagoNewDialog(t *testing.T) {
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		body := sdp.GenerateForAudio(net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 1), 34455, sdp.ModeSendrecv, []string{sdp.FORMAT_TYPE_ALAW})
		return sip.NewResponseFromRequest(req, 200, "OK", body)
	})
	ctx := context.TODO()

	t.Run("CloseNoError", func(t *testing.T) {
		dialog, err := dg.NewDialog(sip.Uri{User: "alice", Host: "localhost"}, NewDialogOptions{})
		require.NoError(t, err)
		dialog.Close()
	})

	// t.Run("NoAcked", func(t *testing.T) {
	// 	dialog, err := dg.NewDialog( sip.Uri{User: "alice", Host: "localhost"}, NewDialogOpts{})
	// 	require.NoError(t, err)
	// 	defer dialog.Close()

	// 	err = dialog.Invite(ctx, InviteOptions{})
	// 	require.NoError(t, err)

	// 	dialog.Audio
	// })

	t.Run("FullDialog", func(t *testing.T) {
		dialog, err := dg.NewDialog(sip.Uri{User: "alice", Host: "localhost"}, NewDialogOptions{})
		require.NoError(t, err)
		defer dialog.Close()

		err = dialog.Invite(ctx, InviteClientOptions{})
		require.NoError(t, err)
		assert.NotEmpty(t, dialog.ID)

		err = dialog.Ack(ctx)
		require.NoError(t, err)

		// assert.NotEmpty(t, dialog.ID)
	})

	// _, err := dg.Invite(context.Background(), sip.Uri{User: "alice", Host: "localhost"}, InviteOptions{})
	// if assert.Error(t, err) {
	// 	assert.Equal(t, "no SDP in response", err.Error())
	// }
}

// TestDiagoInfoWithoutContentType sends real INFO requests to the INFO handler
// over a loopback socket. A body-less INFO is valid SIP and carries no
// Content-Type (RFC 3261 §20.15), so the handler must answer it rather than
// fail on the missing header. A middleware recovers any panic from the handler
// so the failure is reported instead of ending the test binary.
func TestDiagoInfoWithoutContentType(t *testing.T) {
	var mu sync.Mutex
	panics := map[string]string{}
	recoverPanic := func(next sipgo.RequestHandler) sipgo.RequestHandler {
		return func(req *sip.Request, tx sip.ServerTransaction) {
			defer func() {
				if r := recover(); r != nil {
					mu.Lock()
					panics[req.CallID().Value()] = fmt.Sprint(r)
					mu.Unlock()
					_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusInternalServerError, "Server Internal Error", nil))
				}
			}()
			next(req, tx)
		}
	}

	ua, _ := sipgo.NewUA()
	t.Cleanup(func() { _ = ua.Close() })
	dg := NewDiago(ua, WithServerRequestMiddleware(recoverPanic))

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	port := conn.LocalAddr().(*net.UDPAddr).Port
	go func() { _ = dg.server.ServeUDP(conn) }()

	clientUA, _ := sipgo.NewUA()
	t.Cleanup(func() { _ = clientUA.Close() })
	client, err := sipgo.NewClient(clientUA, sipgo.WithClientHostname("127.0.0.1"))
	require.NoError(t, err)

	tests := []struct {
		name        string
		contentType string
		toTag       string
		body        []byte
		wantStatus  int
	}{
		{name: "no Content-Type", wantStatus: sip.StatusNotAcceptable},
		{name: "not DTMF relay", contentType: "text/plain", body: []byte("hello"), wantStatus: sip.StatusNotAcceptable},
		{name: "DTMF relay outside any dialog", contentType: "application/dtmf-relay", toTag: ";tag=nodialog", body: []byte("Signal=1\r\nDuration=100\r\n"), wantStatus: sip.StatusCallTransactionDoesNotExists},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			callID := fmt.Sprintf("info-no-ctype-%d", i)
			req := sip.NewRequest(sip.INFO, sip.Uri{User: "dg", Host: "127.0.0.1", Port: port})
			req.AppendHeader(sip.NewHeader("From", "<sip:peer@127.0.0.1>;tag=peer"))
			req.AppendHeader(sip.NewHeader("To", "<sip:dg@127.0.0.1>"+tc.toTag))
			req.AppendHeader(sip.NewHeader("Call-ID", callID))
			req.AppendHeader(sip.NewHeader("CSeq", "1 INFO"))
			if tc.contentType != "" {
				req.AppendHeader(sip.NewHeader("Content-Type", tc.contentType))
			}
			req.SetBody(tc.body)
			if tc.contentType == "" {
				require.Nil(t, req.ContentType(), "the request must reach the handler without Content-Type")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			res, err := client.Do(ctx, req)
			require.NoError(t, err)

			mu.Lock()
			recovered := panics[callID]
			mu.Unlock()
			require.Empty(t, recovered, "INFO handler panicked")
			assert.Equal(t, tc.wantStatus, res.StatusCode)
		})
	}
}

// TestDiagoFailedRequestWithoutFromOrTo sends real BYE, NOTIFY and INVITE
// requests lacking From or To to the handlers over a loopback socket. Dialog
// matching and INVITE reading fail exactly when either header is missing, so
// the failure path must not read them unchecked. A middleware recovers any
// panic from every handler run so the failure is reported instead of ending
// the test binary, and signals when the first run has returned, because the
// 400 goes out before the failure is logged.
func TestDiagoFailedRequestWithoutFromOrTo(t *testing.T) {
	var mu sync.Mutex
	panics := map[string][]string{}
	handlerDone := map[string]chan struct{}{}
	recoverPanic := func(next sipgo.RequestHandler) sipgo.RequestHandler {
		return func(req *sip.Request, tx sip.ServerTransaction) {
			callID := req.CallID().Value()
			defer func() {
				r := recover()
				mu.Lock()
				if r != nil {
					panics[callID] = append(panics[callID], fmt.Sprint(r))
				}
				done := handlerDone[callID]
				delete(handlerDone, callID)
				mu.Unlock()
				if r != nil {
					_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusInternalServerError, "Server Internal Error", nil))
				}
				if done != nil {
					close(done)
				}
			}()
			next(req, tx)
		}
	}

	ua, _ := sipgo.NewUA()
	t.Cleanup(func() { _ = ua.Close() })
	dg := NewDiago(ua, WithServerRequestMiddleware(recoverPanic))

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	port := conn.LocalAddr().(*net.UDPAddr).Port
	go func() { _ = dg.server.ServeUDP(conn) }()

	clientUA, _ := sipgo.NewUA()
	t.Cleanup(func() { _ = clientUA.Close() })
	client, err := sipgo.NewClient(clientUA, sipgo.WithClientHostname("127.0.0.1"))
	require.NoError(t, err)

	tests := []struct {
		name    string
		method  sip.RequestMethod
		absent  string
		headers []sip.Header
	}{
		{name: "BYE without From", method: sip.BYE, absent: "From"},
		{name: "BYE without To", method: sip.BYE, absent: "To"},
		{name: "NOTIFY without To", method: sip.NOTIFY, absent: "To", headers: []sip.Header{sip.NewHeader("Event", "refer")}},
		{name: "INVITE without From", method: sip.INVITE, absent: "From"},
		{name: "INVITE without To", method: sip.INVITE, absent: "To"},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			callID := fmt.Sprintf("no-from-to-%d", i)
			req := sip.NewRequest(tc.method, sip.Uri{User: "dg", Host: "127.0.0.1", Port: port})
			if tc.absent != "From" {
				req.AppendHeader(sip.NewHeader("From", "<sip:peer@127.0.0.1>;tag=peer"))
			}
			if tc.absent != "To" {
				req.AppendHeader(sip.NewHeader("To", "<sip:dg@127.0.0.1>;tag=dg"))
			}
			req.AppendHeader(sip.NewHeader("Call-ID", callID))
			req.AppendHeader(sip.NewHeader("CSeq", "1 "+tc.method.String()))
			req.AppendHeader(sip.NewHeader("Contact", "<sip:peer@127.0.0.1>"))
			for _, h := range tc.headers {
				req.AppendHeader(h)
			}
			if tc.absent == "From" {
				require.Nil(t, req.From(), "the request must reach the handler without From")
			} else {
				require.Nil(t, req.To(), "the request must reach the handler without To")
			}

			done := make(chan struct{})
			mu.Lock()
			handlerDone[callID] = done
			mu.Unlock()

			isInvite := tc.method == sip.INVITE
			timeout := 5 * time.Second
			if isInvite {
				timeout = time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			res, err := client.Do(ctx, req, sipgo.ClientRequestAddVia)
			if !isInvite {
				require.NoError(t, err)
			}

			select {
			case <-done:
			case <-time.After(5 * time.Second):
				require.FailNow(t, "handler did not return")
			}

			mu.Lock()
			recovered := panics[callID]
			mu.Unlock()
			require.Empty(t, recovered, "%s handler panicked", tc.method)

			if isInvite {
				// ReadInvite rejects the INVITE and the handler sends no final response.
				require.ErrorIs(t, err, context.DeadlineExceeded)
				assert.Nil(t, res)
				return
			}
			assert.Equal(t, sip.StatusBadRequest, res.StatusCode)
		})
	}
}

func TestIntegrationDiagoTransportEmpheralPort(t *testing.T) {
	tran := Transport{
		Transport: "udp",
		BindHost:  "127.0.0.1",
		BindPort:  0,
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()

	dg := NewDiago(ua, WithTransport(tran))

	err := dg.ServeBackground(context.TODO(), func(d *DialogServerSession) {})
	require.NoError(t, err)

	newTran, _ := dg.getTransport("udp")
	t.Log("port assigned", newTran.BindPort)
	assert.NotEmpty(t, newTran.BindPort)
}

func TestIntegrationDiagoCallWithCustomCodecs(t *testing.T) {
	// TODO: USE TLS as transport for more correct test
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	l16Codec := media.Codec{
		Name:        "L16",
		PayloadType: 98,
		SampleRate:  8000,
		SampleDur:   20 * time.Millisecond,
		NumChannels: 1,
	}

	{
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		dg := NewDiago(ua,
			WithTransport(
				Transport{
					ID:        "tcp",
					Transport: "tcp",
					BindHost:  "127.0.0.1",
					BindPort:  15066,
				},
			),
			WithMediaConfig(
				MediaConfig{
					Codecs: []media.Codec{l16Codec, media.CodecAudioAlaw},
				},
			))

		err := dg.ServeBackground(ctx, func(d *DialogServerSession) {
			d.Trying()
			if err := d.Answer(); err != nil {
				panic(err)
			}

			err := d.Echo()
			slog.Info("Echo finished with", "error", err)

		})
		require.NoError(t, err)
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()
	dg := NewDiago(ua,
		WithTransport(
			Transport{
				ID:        "tcp",
				Transport: "tcp",
				BindHost:  "127.0.0.1",
			},
		),
		WithMediaConfig(
			MediaConfig{
				Codecs: []media.Codec{l16Codec, media.CodecAudioAlaw},
			},
		))

	d, err := dg.Invite(ctx, sip.Uri{User: "11", Host: "127.0.0.1", Port: 15066}, InviteOptions{Transport: "tcp"})
	require.NoError(t, err)

	l16Audio := bytes.Repeat([]byte{0, 16, 96, 0}, l16Codec.Samples16()/4)
	reader := bytes.NewBuffer(l16Audio)
	r, _ := d.AudioReader()
	w, _ := d.AudioWriter()
	_, err = media.Copy(reader, w)
	require.ErrorIs(t, err, io.EOF)

	recv := make([]byte, len(l16Audio))
	r.Read(recv)
	assert.Equal(t, l16Audio, recv)
}

func TestIntegrationDiagoSRTPCall(t *testing.T) {
	// TODO: USE TLS as transport for more correct test
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	{
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		dg := NewDiago(ua,
			WithTransport(
				Transport{
					ID:        "tcp",
					Transport: "tcp",
					BindHost:  "127.0.0.1",
					BindPort:  15443,
					MediaSRTP: 1, // This enables SRTP
				},
			),
			WithMediaConfig(
				MediaConfig{
					Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
				},
			))

		err := dg.ServeBackground(ctx, func(d *DialogServerSession) {
			d.Trying()
			if err := d.Answer(); err != nil {
				panic(err)
			}

			err := d.Echo()
			slog.Info("Echo finished with", "error", err)

		})
		require.NoError(t, err)
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()
	dg := NewDiago(ua,
		WithTransport(
			Transport{
				ID:        "tcp",
				Transport: "tcp",
				BindHost:  "127.0.0.1",
				BindPort:  15441,
				MediaSRTP: 1, // USE SRTP
			},
		),
		WithMediaConfig(
			MediaConfig{
				Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
			},
		))

	// err = dg.ServeBackground(ctx, func(d *DialogServerSession) {})
	// require.NoError(t, err)

	d, err := dg.Invite(ctx, sip.Uri{User: "11", Host: "127.0.0.1", Port: 15443}, InviteOptions{Transport: "tcp"})
	require.NoError(t, err)

	// pb, err := d.PlaybackCreate()
	// if err != nil {
	// 	panic(err)
	// }

	ulaw := make([]byte, 160)
	audio.EncodeUlawTo(ulaw, bytes.Repeat([]byte{1}, 320))

	reader := bytes.NewBuffer(ulaw)
	r, _ := d.AudioReader()
	w, _ := d.AudioWriter()
	_, err = media.Copy(reader, w)
	require.ErrorIs(t, err, io.EOF)

	recv := make([]byte, 160)
	r.Read(recv)
	assert.Equal(t, ulaw, recv)
}

func TestIntegrationDiagoDTLSCall(t *testing.T) {
	// TODO: USE TLS as transport for more correct test
	// media.DTLSDebug = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	{
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		dg := NewDiago(ua,
			WithTransport(
				Transport{
					ID:        "tcp",
					Transport: "tcp",
					BindHost:  "127.0.0.1",
					BindPort:  16443,
					MediaSRTP: 2, // This enables SRTP DTLS
					MediaDTLSConf: media.DTLSConfig{
						Certificates:     []tls.Certificate{testdata.ServerCertificate()},
						ServerClientAuth: media.ServerClientAuthNoCert,
					},
				},
			),
			WithMediaConfig(
				MediaConfig{
					Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
				},
			))

		err := dg.ServeBackground(ctx, func(d *DialogServerSession) {
			d.Trying()
			if err := d.Answer(); err != nil {
				panic(err)
			}

			err := d.Echo()
			slog.Info("Echo finished with", "error", err)

		})
		require.NoError(t, err)
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()
	dg := NewDiago(ua,
		WithTransport(
			Transport{
				ID:        "tcp",
				Transport: "tcp",
				BindHost:  "127.0.0.1",
				BindPort:  16441,
				MediaSRTP: 2, // USE DTLS
				// We do not need any Certificate verification
				MediaDTLSConf: media.DTLSConfig{},
			},
		),
		WithMediaConfig(
			MediaConfig{
				Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
			},
		))

	// err = dg.ServeBackground(ctx, func(d *DialogServerSession) {})
	// require.NoError(t, err)

	d, err := dg.Invite(ctx, sip.Uri{User: "11", Host: "127.0.0.1", Port: 16443}, InviteOptions{Transport: "tcp"})
	require.NoError(t, err)

	// pb, err := d.PlaybackCreate()
	// if err != nil {
	// 	panic(err)
	// }

	ulaw := make([]byte, 160)
	audio.EncodeUlawTo(ulaw, bytes.Repeat([]byte{1}, 320))

	reader := bytes.NewBuffer(ulaw)
	r, _ := d.AudioReader()
	w, _ := d.AudioWriter()
	time.Sleep(1 * time.Second)
	t.Log("---------------------------Writing media")
	_, err = media.Copy(reader, w)
	require.ErrorIs(t, err, io.EOF)

	recv := make([]byte, 160)
	r.Read(recv)
	assert.Equal(t, ulaw, recv)
}
