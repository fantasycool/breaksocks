package server

import (
	gocrypto "crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"fmt"
	"github.com/breaksocks/breaksocks/crypto"
	"github.com/breaksocks/breaksocks/protocol"
	"github.com/breaksocks/breaksocks/session"
	"github.com/breaksocks/breaksocks/utils"
	"io"
	"log"
	"math/big"
	"net"
	"os"
	"strings"
	"sync"
)

type Server struct {
	sessions  *session.SessionManager
	config    *utils.ServerConfig
	user_cfgs *UserConfigs

	priv_key    *rsa.PrivateKey
	pub_der     []byte
	g_cipher    *crypto.GlobalCipherConfig
	enc_methods []byte

	listenser *net.TCPListener
}

func NewServer(config *utils.ServerConfig) (*Server, error) {
	server := new(Server)
	var err error

	if len(config.LinkEncryptMethods) == 0 {
		return nil, fmt.Errorf("encrypt methods can't be empty")
	}
	server.enc_methods = []byte(strings.Join(config.LinkEncryptMethods, ","))

	if server.priv_key, err = crypto.LoadRSAPrivateKey(config.KeyPath); err != nil {
		if os.IsNotExist(err) {
			log.Printf("generating new private key(RSA 2048bits) ...")
			if server.priv_key, err = crypto.GenerateRSAKey(2048, config.KeyPath); err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}
	if server.pub_der, err = x509.MarshalPKIXPublicKey(&server.priv_key.PublicKey); err != nil {
		return nil, err
	}

	if config.GlobalEncryptMethod != "" {
		if server.g_cipher, err = crypto.LoadGlobalCipherConfig(
			config.GlobalEncryptMethod, []byte(config.GlobalEncryptPassword)); err != nil {
			return nil, err
		}
	}

	if server.user_cfgs, err = GetUserConfigs(config.UserConfigPath); err != nil {
		return nil, err
	}

	if l, err := net.Listen("tcp", config.ListenAddr); err == nil {
		server.listenser = l.(*net.TCPListener)
		log.Printf("listen on: %s", config.ListenAddr)
	} else {
		return nil, err
	}

	server.sessions = session.NewSessionManager()
	server.config = config
	return server, nil
}

func (ser *Server) Run() {
	for {
		if conn, err := ser.listenser.AcceptTCP(); err != nil {
			log.Fatalf("accept fail: %s", err.Error())
		} else {
			go ser.processClient(conn)
		}
	}
}

func (ser *Server) processClient(conn *net.TCPConn) {
	defer conn.Close()

	pipe := crypto.NewStreamPipe(conn)
	if ser.g_cipher != nil {
		enc, dec, err := ser.g_cipher.NewCipher()
		if err != nil {
			log.Printf("kl: %d, ivl: %d, %#v", len(ser.g_cipher.Key), len(ser.g_cipher.IV),
				ser.g_cipher.Config)
			log.Fatalf("make global enc/dec fail: %s", err.Error())
		}
		pipe.SwitchCipher(enc, dec)
	}
	if err := conn.SetNoDelay(true); err != nil {
		log.Fatalf("set client NoDelay fail: %s", err.Error())
	}

	user := ser.clientStartup(pipe)
	if user == nil {
		return
	}
	ser.clientLoop(user, pipe)
}

func (ser *Server) clientStartup(pipe *crypto.StreamPipe) *session.Session {
	// cipher exchange && session cipher switch
	header := make([]byte, 4)
	if _, err := io.ReadFull(pipe, header); err != nil {
		log.Printf("receive startup header fail: %s", err.Error())
		return nil
	}

	if header[0] != protocol.PROTO_MAGIC {
		log.Printf("reveiced a invalid magic: %d", header[0])
		return nil
	}

	if header[1] == 0 {
		return ser.newSession(pipe)
	}
	if header[2] == 0 || header[3] == 0 {
		log.Printf("reuse session, 0 random/hmac")
		return nil
	}

	body_size := header[1] + header[2] + header[3]
	body := make([]byte, body_size)
	if _, err := io.ReadFull(pipe, body); err != nil {
		log.Printf("receive startup body fail")
		return nil
	}
	return ser.reuseSession(pipe, body[:header[1]],
		body[header[1]:header[1]+header[2]],
		body[header[1]+header[2]:])
}

func (ser *Server) newSession(pipe *crypto.StreamPipe) *session.Session {
	ctx, err := crypto.NewCipherContext(5)
	if err != nil {
		log.Printf("create cipher context fail: %s", err.Error())
		return nil
	}

	f, err := ctx.MakeF()
	if err != nil {
		log.Printf("make f fail: %s", err.Error())
	}
	p_bs, f_bs := ctx.P.Bytes(), f.Bytes()

	buf := make([]byte, len(ser.pub_der)+len(p_bs)+len(f_bs)+len(ser.enc_methods)+2048)
	utils.WriteN2(buf, uint16(len(ser.pub_der)))
	utils.WriteN2(buf[2:], uint16(len(p_bs)))
	utils.WriteN2(buf[4:], uint16(len(f_bs)))
	utils.WriteN2(buf[8:], uint16(len(ser.enc_methods)))
	cur := 10
	cur += copy(buf[cur:], ser.pub_der)
	cur += copy(buf[cur:], p_bs)
	buf[cur] = byte(ctx.G)
	cur += 1
	cur += copy(buf[cur:], f_bs)

	hash_bs := sha256.Sum256(buf[10+len(ser.pub_der) : cur])
	if sig, err := rsa.SignPKCS1v15(rand.Reader, ser.priv_key, gocrypto.SHA256,
		hash_bs[:]); err != nil {
		log.Printf("sign p/g/f fail: %s", err.Error())
		return nil
	} else {
		utils.WriteN2(buf[6:], uint16(len(sig)))
		cur += copy(buf[cur:], sig)
	}
	cur += copy(buf[cur:], ser.enc_methods)

	if _, err := pipe.Write(buf[:cur]); err != nil {
		log.Printf("write pipe fail: %s", err.Error())
		return nil
	}

	// finihs cipher exchange
	if _, err := io.ReadFull(pipe, buf[:4]); err != nil {
		log.Printf("read cipher exchange finish fail: %s", err.Error())
		return nil
	}
	e_size := utils.ReadN2(buf)
	md_size := utils.ReadN2(buf[2:])
	if e_size == 0 || md_size < 0 || e_size+md_size > uint16(len(buf)) {
		log.Printf("invalid e/md size:%d %d", e_size, md_size)
		return nil
	}
	if _, err := io.ReadFull(pipe, buf[:e_size+md_size]); err != nil {
		log.Printf("read cipher exchange finish body fail: %s", err.Error())
		return nil
	}
	method := string(buf[e_size : e_size+md_size])
	var cipher_cfg *crypto.CipherConfig
	for _, md := range ser.config.LinkEncryptMethods {
		if md == method {
			cipher_cfg = crypto.GetCipherConfig(method)
			break
		}
	}
	if cipher_cfg == nil {
		log.Printf("invalid method: %s", method)
		return nil
	}
	ctx.CalcKey(new(big.Int).SetBytes(buf[:e_size]))
	key, iv := ctx.MakeCryptoKeyIV(cipher_cfg.KeySize, cipher_cfg.IVSize)
	if enc, dec, err := cipher_cfg.NewCipher(key, iv); err != nil {
		log.Printf("new stream cipher fail: %s", err.Error())
		return nil
	} else {
		pipe.SwitchCipher(enc, dec)
	}

	s := ser.clientLogin(pipe)
	if s != nil {
		s.CipherCtx = ctx
		s.CipherConfig = cipher_cfg
	}
	return s
}

func (ser *Server) clientLogin(pipe *crypto.StreamPipe) *session.Session {
	buf := make([]byte, 4+32+32)
	if _, err := io.ReadFull(pipe, buf[:4]); err != nil {
		log.Printf("receive login req fail: %s", err.Error())
		return nil
	}

	// rep
	login_ok := protocol.B_FALSE
	var msg []byte
	var s *session.Session

	user_size, passwd_size := buf[2], buf[3]
	if user_size > 0 && user_size <= 32 && passwd_size > 0 && passwd_size <= 32 {
		if _, err := io.ReadFull(pipe, buf[:user_size+passwd_size]); err != nil {
			log.Printf("read login body fail: %s", err.Error())
			return nil
		}
		user, passwd := string(buf[:user_size]), buf[user_size:user_size+passwd_size]
		user_cfg := ser.user_cfgs.Get(user)
		if user_cfg == nil || user_cfg.Password != string(passwd) {
			msg = []byte("invalid username/password")
		} else {
			login_ok = protocol.B_TRUE
			var err error
			if s, err = ser.sessions.NewSession(); err != nil {
				log.Printf("new session fail: %s", err.Error())
				return nil
			}
			s.Username = string(user)
			if msg, err = s.Id.Bytes(); err != nil {
				log.Printf("sessionId toBytes fail: %s", err.Error())
				return nil
			}
		}
	} else {
		msg = []byte("user/passwd size invalid")
	}

	utils.WriteN2(buf, protocol.PROTO_VERSION)
	buf[2] = login_ok
	buf[3] = byte(len(msg))
	copy(buf[4:], msg)
	if _, err := pipe.Write(buf[:4+buf[3]]); err != nil {
		log.Printf("write err rep fail: %s", err.Error())
		return nil
	}
	return s
}

func CheckMAC(message, messageMAC, key []byte) bool {
	mac := hmac.New(sha256.New, key)
	mac.Write(message)
	expectedMAC := mac.Sum(nil)
	return hmac.Equal(messageMAC, expectedMAC)
}

func (ser *Server) reuseSession(pipe *crypto.StreamPipe, s_bs, rand_bs, hmac_bs []byte) *session.Session {
	sessionId := session.SessionIdFromBytes(s_bs)
	s := ser.sessions.GetSession(sessionId)
	if s == nil {
		return nil
	}

	do_init := false
	rep := []byte{protocol.B_TRUE, protocol.REUSE_SUCCESS}
	if !CheckMAC(rand_bs, hmac_bs, s.CipherCtx.CryptoKey) {
		rep[0] = protocol.B_FALSE
		rep[1] = protocol.REUSE_FAIL_START_CIPHER_EXCHANGE | protocol.REUSE_FAIL_HMAC_FAIL
		do_init = true
	}

	if _, err := pipe.Write(rep); err != nil {
		log.Printf("write init rep fail: %s", err.Error())
		return nil
	}
	if do_init {
		return ser.newSession(pipe)
	}
	return s
}

func (ser *Server) clientLoop(user *session.Session, pipe *crypto.StreamPipe) {
	log.Printf("start proxy: %s(%s)", user.Username, user.Id)
	write_ch := make(chan []byte)
	go func() {
		for {
			if data, ok := <-write_ch; ok {
				if _, err := pipe.Write(data); err != nil {
					log.Printf("write to client fail: %s", err.Error())
				}
			}
		}
	}()

	conns := make(map[uint32]chan []byte)
	var lock sync.RWMutex
	buf := make([]byte, 65535)
	for {
		if _, err := io.ReadFull(pipe, buf[:4]); err != nil {
			log.Printf("recv packet fail: %s", err.Error())
			return
		} else {
			if buf[0] != protocol.PROTO_MAGIC {
				log.Printf("invalid magic: %d", buf[0])
				return
			}
			pkt_size := utils.ReadN2(buf[2:])
			if _, err := io.ReadFull(pipe, buf[4:pkt_size+4]); err != nil {
				log.Printf("recv packet fail: %s", err.Error())
				return
			}
			switch buf[1] {
			case protocol.PACKET_PROXY:
				conn_id := utils.ReadN4(buf[4:])
				lock.RLock()
				ch := conns[conn_id]
				lock.RUnlock()
				if ch != nil {
					ch <- utils.Dump(buf[8 : pkt_size+4])
				} else {
					log.Printf("no such conn: %d", conn_id)
				}
			case protocol.PACKET_NEW_CONN:
				port := utils.ReadN2(buf[6:])
				conn_id := utils.ReadN4(buf[8:])
				conn_type := buf[4]
				addr := utils.Dump(buf[12 : 12+int(buf[5])])
				read := make(chan []byte, 32)
				lock.Lock()
				conns[conn_id] = read
				lock.Unlock()
				go func() {
					ser.copyRemote(read, write_ch, conn_id, conn_type, addr, port)
					lock.Lock()
					delete(conns, conn_id)
					lock.Unlock()

					buf := make([]byte, 8)
					buf[0] = protocol.PROTO_MAGIC
					buf[1] = protocol.PACKET_CLOSE_CONN
					utils.WriteN2(buf[2:], 4)
					utils.WriteN4(buf[4:], conn_id)
					write_ch <- buf
				}()
			case protocol.PACKET_CLOSE_CONN:
				conn_id := utils.ReadN4(buf[4:])
				lock.Lock()
				ch := conns[conn_id]
				if ch != nil {
					close(ch)
					delete(conns, conn_id)
				}
				lock.Unlock()
			}
		}
	}
}

func (ser *Server) copyRemote(read, write chan []byte, conn_id uint32, conn_type byte, addr []byte, port uint16) {
	var rconn *net.TCPConn
	if conn_type == protocol.PROTO_ADDR_IP {
		var remote_addr net.TCPAddr
		remote_addr.IP = net.IP(addr)
		remote_addr.Port = int(port)
		log.Printf("addr: %v %v", addr, remote_addr)
		if conn, err := net.DialTCP("tcp", nil, &remote_addr); err == nil {
			rconn = conn
		} else {
			log.Printf("conn %s fail: %s", remote_addr, err.Error())
		}
	} else {
		raddr := net.JoinHostPort(string(addr), fmt.Sprintf("%d", port))
		if conn, err := net.Dial("tcp", raddr); err == nil {
			rconn = conn.(*net.TCPConn)
		} else {
			log.Printf("conn %s fail: %s", raddr, err.Error())
		}
	}
	if rconn == nil {
		return
	}

	buf := make([]byte, 65535)
	buf[0] = protocol.PROTO_MAGIC
	buf[1] = protocol.PACKET_PROXY
	utils.WriteN4(buf[4:], conn_id)

	remote_ch := make(chan int)
	go func() {
		recv_buf := buf[8:]
		for {
			if n, err := rconn.Read(recv_buf); err == nil {
				log.Printf("recv from remote: %d", n)
				remote_ch <- n
			} else {
				log.Printf("remote closed")
				remote_ch <- 0
				return
			}
		}
	}()

	for {
		select {
		case data, ok := <-read:
			log.Printf("from cli: %v", data, ok)
			if !ok {
				rconn.Close()
				return
			}
			if _, err := rconn.Write(data); err != nil {
				rconn.Close()
				return
			} else {
				log.Printf("write remote ok")
			}
		case n := <-remote_ch:
			if n == 0 {
				rconn.Close()
				return
			}
			utils.WriteN2(buf[2:], uint16(n+4))
			write <- utils.Dump(buf[:n+8])
		}
	}
}
