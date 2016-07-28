package proxy

import (
	"bufio"
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/yinqiwen/gsnova/common/event"
	"github.com/yinqiwen/gsnova/common/fakecert"
	"github.com/yinqiwen/gsnova/local/socks"
)

var seed uint32 = 0
var proxyServerRunning = true

func serveProxyConn(conn net.Conn, proxy ProxyConfig) {
	var p Proxy
	proxyName := ""
	sid := atomic.AddUint32(&seed, 1)
	queue := event.NewEventQueue()
	connClosed := false
	session := newProxySession(sid, queue)
	defer closeProxySession(sid)

	findProxy := func(req *http.Request) {
		if nil == p {
			for _, pac := range proxy.PAC {
				if pac.Match(req) {
					p = getProxyByName(pac.Remote)
					proxyName = pac.Remote
					break
				}

			}
			if nil == p {
				log.Printf("No proxy found.")
				return
			}
		}
	}
	//isSocksConn := false
	socksConn, bufconn, err := socks.NewSocksConn(conn)
	if nil == err {
		//isSocksConn = true
		socksConn.Grant(&net.TCPAddr{
			IP: net.ParseIP("0.0.0.0"), Port: 0})
		conn = socksConn
		creq, _ := http.NewRequest("Connect", socksConn.Req.Target, nil)
		findProxy(creq)
		session.Hijacked = true
		tcpOpen := &event.TCPOpenEvent{}
		tcpOpen.SetId(sid)
		tcpOpen.Addr = socksConn.Req.Target
		p.Serve(session, tcpOpen)
	} else {
		if nil == bufconn {
			conn.Close()
			return
		}
	}
	if nil == bufconn {
		bufconn = bufio.NewReader(conn)
	}
	defer conn.Close()

	go func() {
		for !connClosed {
			ev, err := queue.Read(1 * time.Second)
			if err != nil {
				if err != io.EOF {
					continue
				}
				return
			}
			//log.Printf("Session:%d recv event:%T", sid, ev)
			switch ev.(type) {
			case *event.ErrorEvent:
				err := ev.(*event.ErrorEvent)
				log.Printf("[ERROR]Session:%d recv error %d:%s", err.Code, err.Reason)
				conn.Close()
				return
			case *event.TCPCloseEvent:
				conn.Close()
				return
			case *event.TCPChunkEvent:
				conn.Write(ev.(*event.TCPChunkEvent).Content)
			case *event.HTTPResponseEvent:
				ev.(*event.HTTPResponseEvent).Write(conn)
				code := ev.(*event.HTTPResponseEvent).StatusCode
				log.Printf("Session:%d response:%d %v", ev.GetId(), code, http.StatusText(int(code)))
			default:
				log.Printf("Invalid event type:%T to process", ev)
			}
		}
	}()

	// sniSniffed := false
	// tmp := make([]byte, 0)
	for {
		if session.Hijacked {
			buffer := make([]byte, 8192)
			n, err := bufconn.Read(buffer)
			if nil != err {
				log.Printf("Session:%d read chunk failed from proxy connection:%v", sid, err)
				break
			}
			// if !sniSniffed {
			// 	tmp = append(tmp, buffer[0:n]...)
			// 	sni, err := helper.TLSParseSNI(tmp)
			// 	if err != nil {
			// 		if err != helper.ErrTLSIncomplete {
			// 			sniSniffed = true
			// 			log.Printf("####%v", err)
			// 		}
			// 	} else {
			// 		sniSniffed = true
			// 		log.Printf("####SNI = %v", sni)
			// 	}
			// }
			var chunk event.TCPChunkEvent
			chunk.SetId(sid)
			chunk.Content = buffer[0:n]
			p.Serve(session, &chunk)
			continue
		}
		req, err := http.ReadRequest(bufconn)
		if nil != err {
			log.Printf("Session:%d read request failed from proxy connection:%v", sid, err)
			break
		}
		findProxy(req)
		reqUrl := req.URL.String()
		if strings.EqualFold(req.Method, "Connect") {
			reqUrl = req.URL.Host
		} else {
			if !strings.HasPrefix(reqUrl, "http://") && !strings.HasPrefix(reqUrl, "https://") {
				if session.SSLHijacked {
					reqUrl = "https://" + req.Host + reqUrl
				} else {
					reqUrl = "http://" + req.Host + reqUrl
				}
			}
		}
		log.Printf("[%s]Session:%d request:%s %v", proxyName, sid, req.Method, reqUrl)

		req.Header.Del("Proxy-Connection")
		ev := event.NewHTTPRequestEvent(req)
		ev.SetId(sid)
		maxBody := p.Features().MaxRequestBody
		if maxBody > 0 && req.ContentLength > 0 {
			if int64(maxBody) < req.ContentLength {
				log.Printf("[ERROR]Too large request:%d for limit:%d", req.ContentLength, maxBody)
				return
			}
			for int64(len(ev.Content)) < req.ContentLength {
				buffer := make([]byte, 8192)
				n, err := req.Body.Read(buffer)
				if nil != err {
					break
				}
				ev.Content = append(ev.Content, buffer[0:n]...)
			}
		}

		p.Serve(session, ev)
		if maxBody < 0 && req.ContentLength != 0 {
			for nil != req.Body {
				buffer := make([]byte, 8192)
				n, err := req.Body.Read(buffer)
				if nil != err {
					break
				}
				var chunk event.TCPChunkEvent
				chunk.SetId(sid)
				chunk.Content = buffer[0:n]
				p.Serve(session, &chunk)
			}
		}
		if strings.EqualFold(req.Method, "Connect") && (session.SSLHijacked || session.Hijacked) {
			conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		}
		if session.SSLHijacked {
			if tlsconn, ok := conn.(*tls.Conn); !ok {
				tlscfg, err := fakecert.TLSConfig(req.Host)
				if nil != err {
					log.Printf("[ERROR]Failed to generate fake cert for %s:%v", req.Host, err)
					return
				}
				tlsconn = tls.Server(conn, tlscfg)
				conn = tlsconn
				bufconn = bufio.NewReader(conn)
			}
		}
	}
	if nil != p {
		tcpclose := &event.TCPCloseEvent{}
		tcpclose.SetId(sid)
		p.Serve(session, tcpclose)
	}
	connClosed = true
}

func startLocalProxyServer(proxy ProxyConfig) (*net.TCPListener, error) {
	tcpaddr, err := net.ResolveTCPAddr("tcp", proxy.Local)
	if nil != err {
		log.Fatalf("[ERROR]Local server address:%s error:%v", proxy.Local, err)
		return nil, err
	}
	var lp *net.TCPListener
	lp, err = net.ListenTCP("tcp", tcpaddr)
	if nil != err {
		log.Fatalf("Can NOT listen on address:%s", proxy.Local)
		return nil, err
	}
	log.Printf("Listen on address %s", proxy.Local)
	go func() {
		for proxyServerRunning {
			conn, err := lp.AcceptTCP()
			if nil != err {
				continue
			}
			go serveProxyConn(conn, proxy)
		}
		lp.Close()
	}()
	return lp, nil
}

var runningServers []*net.TCPListener

func startLocalServers() error {
	proxyServerRunning = true
	runningServers = make([]*net.TCPListener, 0)
	for _, proxy := range GConf.Proxy {
		l, _ := startLocalProxyServer(proxy)
		if nil != l {
			runningServers = append(runningServers, l)
		}
	}
	return nil
}

func stopLocalServers() {
	proxyServerRunning = false
	for _, l := range runningServers {
		l.Close()
	}
}