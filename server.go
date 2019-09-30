package relaybaton

import (
	"bytes"
	"compress/flate"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"github.com/gorilla/websocket"
	"github.com/iyouport-org/doh-go"
	"github.com/iyouport-org/doh-go/dns"
	"github.com/iyouport-org/socks5"
	log "github.com/sirupsen/logrus"
	"io/ioutil"
	"net"
	"net/http"
	"strconv"
	"time"
)

// Server of relaybaton
type Server struct {
	peer
}

// NewServer creates a new server using the given config and websocket connection.
func NewServer(conf Config, wsConn *websocket.Conn) *Server {
	server := &Server{}
	server.init(conf)
	server.wsConn = wsConn

	return server
}

// Run start a server
func (server *Server) Run() {
	go server.peer.processQueue()

	for {
		select {
		case <-server.close:
			return
		default:
			server.mutexWsRead.Lock()
			_, content, err := server.wsConn.ReadMessage()
			if err != nil {
				log.Error(err)
				err = server.Close()
				if err != nil {
					log.Error(err)
				}
				return
			}
			go server.handleWsReadServer(content)
		}
	}
}

func (server *Server) handleWsReadServer(content []byte) {
	b := make([]byte, len(content))
	copy(b, content)
	server.mutexWsRead.Unlock()
	var session uint16
	prefix := binary.BigEndian.Uint16(b[:2])
	switch prefix {
	case 0: //delete
		session = binary.BigEndian.Uint16(b[2:])
		server.delete(session)

	case uint16(socks5.ATYPIPv4), uint16(socks5.ATYPDomain), uint16(socks5.ATYPIPv6):
		session = binary.BigEndian.Uint16(b[2:4])
		dstPort := strconv.Itoa(int(binary.BigEndian.Uint16(b[4:6])))
		ipVer := b[1]
		var dstAddr net.IP
		wsw := server.getWebsocketWriter(session)
		if prefix != uint16(socks5.ATYPDomain) {
			dstAddr = b[6:]
		} else {
			var err error
			dstAddr, ipVer, err = nsLookup(bytes.NewBuffer(b[7:]).String())
			if err != nil {
				log.Error(err)
				reply := socks5.NewReply(socks5.RepHostUnreachable, ipVer, net.IPv4zero, []byte{0, 0})
				_, err = wsw.writeReply(*reply)
				if err != nil {
					log.Error(err)
				}
				return
			}
		}
		conn, err := net.Dial("tcp", net.JoinHostPort(dstAddr.String(), dstPort))
		if err != nil {
			log.Error(err)
			reply := socks5.NewReply(socks5.RepServerFailure, ipVer, net.IPv4zero, []byte{0, 0})
			_, err = wsw.writeReply(*reply)
			if err != nil {
				log.Error(err)
			}
			return
		}
		_, addr, port, err := socks5.ParseAddress(conn.LocalAddr().String())
		if err != nil {
			log.Error(err)
			return
		}
		reply := socks5.NewReply(socks5.RepSuccess, ipVer, addr, port)
		_, err = wsw.writeReply(*reply)
		if err != nil {
			log.Error(err)
			return
		}

		server.connPool.set(session, &conn)
		go server.peer.forward(session)

	default:
		session := prefix
		server.receive(session, b[2:])
	}
}

// Handler pass config to ServeHTTP()
type Handler struct {
	Conf Config
}

// ServerHTTP accept incoming HTTP request, establish websocket connections, and a new server for handling the connection. If authentication failed, the request will be redirected to the website set in the configuration file.
func (handler Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{
		EnableCompression: true,
	}
	err := handler.authenticate(r.Header)
	if err != nil {
		log.Error(err)
		handler.redirect(&w, r)
		return
	}
	wsConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Error(err)
		handler.redirect(&w, r)
		return
	}
	wsConn.EnableWriteCompression(true)
	err = wsConn.SetCompressionLevel(flate.BestCompression)
	if err != nil {
		log.Error(err)
		return
	}
	server := NewServer(handler.Conf, wsConn)
	go server.Run()
}

func (handler Handler) authenticate(header http.Header) error {
	username := header.Get("username")
	auth := header.Get("auth")
	cipherText, err := hex.DecodeString(auth)
	if err != nil {
		log.Error(err)
		return err
	}
	h := sha256.New()
	h.Write([]byte(handler.getPassword(username)))
	key := h.Sum(nil)
	block, err := aes.NewCipher(key)
	if err != nil {
		log.Error(err)
		return err
	}
	if len(cipherText) < aes.BlockSize {
		err = errors.New("ciphertext too short")
		log.Error(err)
		return err
	}
	iv := cipherText[:aes.BlockSize]
	cipherText = cipherText[aes.BlockSize:]
	stream := cipher.NewCFBDecrypter(block, iv)
	stream.XORKeyStream(cipherText, cipherText)
	plaintext, err := strconv.ParseInt(string(cipherText), 2, 64)
	if err != nil {
		log.Error(err)
		return err
	}
	if time.Since(time.Unix(plaintext, 0)).Seconds() > 60 {
		err = errors.New("authentication fail")
		log.Error(err)
		return err
	}
	return nil
}

func (handler Handler) getPassword(username string) string {
	//TODO
	return handler.Conf.Client.Password
}

func (handler Handler) redirect(w *http.ResponseWriter, r *http.Request) {
	newReq, err := http.NewRequest(r.Method, "https://"+handler.Conf.Server.Pretend+r.RequestURI, r.Body)
	if err != nil {
		log.Error(err)
		return
	}
	for k, v := range r.Header {
		newReq.Header.Set(k, v[0])
	}
	resp, err := http.DefaultClient.Do(newReq)
	if err != nil {
		log.Error(err)
		return
	}
	for k, v := range resp.Header {
		(*w).Header().Set(k, v[0])
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Error(err)
		return
	}
	err = resp.Body.Close()
	if err != nil {
		log.Error(err)
		return
	}
	_, err = (*w).Write(body)
	if err != nil {
		log.Error(err)
		return
	}
}

func nsLookup(domain string) (net.IP, byte, error) {
	var dstAddr net.IP
	dstAddr = nil

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := doh.New(doh.CloudflareProvider)

	//IPv6
	rsp, err := c.Query(ctx, dns.Domain(domain), dns.TypeAAAA)
	if err != nil {
		log.Error(err)
		return nil, 0, err
	}
	answer := rsp.Answer
	for _, v := range answer {
		if v.Type == 28 {
			dstAddr = net.ParseIP(v.Data).To16()
		}
	}
	if dstAddr != nil {
		return dstAddr, socks5.ATYPIPv6, nil
	}

	//IPv4
	rsp, err = c.Query(ctx, dns.Domain(domain), dns.TypeA)
	if err != nil {
		log.Error(err)
		return nil, 0, err
	}
	answer = rsp.Answer
	for _, v := range answer {
		if v.Type == 1 {
			dstAddr = net.ParseIP(v.Data).To4()
		}
	}
	if dstAddr != nil {
		return dstAddr, socks5.ATYPIPv4, nil
	}

	err = errors.New("DNS error")
	return dstAddr, 0, err
}
