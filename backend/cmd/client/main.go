// gsc-client: 本地客户端。内嵌与服务端相同的 Web UI；上传/下载由本进程直连
// 百度网盘执行（读写本地磁盘），浏览器只查看进度。账号与元数据始终保存在
// 服务端；服务端地址可通过 -server 指定，也可以启动后在网页中设置。
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"

	"gosocode/internal/clientd"
)

// consoleURL turns a listen address into an address a browser can open
// (wildcard/empty hosts become 127.0.0.1).
func consoleURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	server := flag.String("server", os.Getenv("GSC_SERVER"), "服务端地址，例如 http://192.168.1.10:8080；留空则在网页中设置")
	listen := flag.String("listen", "127.0.0.1:8190", "本地监听地址")
	controlKey := flag.String("control-key", os.Getenv("GSC_AGENT_CONTROL_KEY"), "客户端控制面加密密钥（与服务端 GSC_AGENT_CONTROL_KEY 一致）")
	window := flag.Int("window", 8, "上传并发分片窗口（本机直连百度）")
	dlConns := flag.Int("dl-conns", 4, "下载并发连接数（本机直连百度，最多 8）")
	webui := flag.String("webui", "", "本地静态 Web UI 目录；留空使用内嵌页面")
	flag.Parse()

	d, err := clientd.New(clientd.Options{
		Server: *server, Window: *window, DLConns: *dlConns, WebUI: *webui, ControlKey: *controlKey,
	})
	if err != nil {
		log.Fatalf("客户端启动失败: %v", err)
	}

	// Web console entry gets its own banner so it stands out from the
	// surrounding timestamped logs.
	fmt.Fprintf(log.Writer(), "\n"+
		"============================================================\n"+
		"  GoSoCode 本地客户端已启动\n"+
		"\n"+
		"  Web 控制台:  %s\n"+
		"\n"+
		"  上传/下载由本机直连百度执行，浏览器仅查看进度\n"+
		"============================================================\n\n", consoleURL(*listen))
	if err := http.ListenAndServe(*listen, d.Handler()); err != nil {
		log.Fatalf("client: %v", err)
	}
}
