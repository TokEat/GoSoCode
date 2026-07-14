// gsc-client: 本地客户端。内嵌与服务端相同的 Web UI；上传/下载由本进程直连
// 百度网盘执行（读写本地磁盘），浏览器只查看进度。账号与元数据始终保存在
// 服务端；服务端地址可通过 -server 指定，也可以启动后在网页中设置。
package main

import (
	"flag"
	"log"
	"net/http"
	"os"

	"gosocode/internal/clientd"
)

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	server := flag.String("server", os.Getenv("GSC_SERVER"), "服务端地址，例如 http://192.168.1.10:8080；留空则在网页中设置")
	listen := flag.String("listen", ":8190", "本地监听地址")
	window := flag.Int("window", 8, "上传并发分片窗口（本机直连百度）")
	dlConns := flag.Int("dl-conns", 4, "下载并发连接数（本机直连百度，最多 8）")
	webui := flag.String("webui", "", "本地静态 Web UI 目录；留空使用内嵌页面")
	flag.Parse()

	d, err := clientd.New(clientd.Options{
		Server: *server, Window: *window, DLConns: *dlConns, WebUI: *webui,
	})
	if err != nil {
		log.Fatalf("客户端启动失败: %v", err)
	}

	log.Printf("本地入口 http://localhost%s（上传/下载由本机直连百度执行，浏览器仅查看进度）", *listen)
	if err := http.ListenAndServe(*listen, d.Handler()); err != nil {
		log.Fatalf("client: %v", err)
	}
}
