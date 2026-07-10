// gsc-client: 本地客户端。绑定与服务端相同的 Web UI，但上传/下载数据面由本机
// 直连百度网盘（不经过服务端），账号与元数据始终保存在服务端。
package main

import (
	"flag"
	"log"
	"net/http"
	"os"

	"gosocode/internal/clientd"
)

func main() {
	server := flag.String("server", os.Getenv("GSC_SERVER"), "服务端地址（必填），例如 http://192.168.1.10:8080")
	listen := flag.String("listen", ":8090", "本地监听地址")
	window := flag.Int("window", 8, "上传并发分片窗口（本机直连百度）")
	dlConns := flag.Int("dl-conns", 4, "下载并发连接数（本机直连百度，最多 8）")
	webui := flag.String("webui", "", "本地静态 Web UI 目录；留空则代理服务端页面")
	flag.Parse()

	if *server == "" {
		log.Fatal("请通过 -server（或环境变量 GSC_SERVER）指定服务端地址：客户端必须先连接服务端才能使用")
	}

	d, err := clientd.New(clientd.Options{
		Server: *server, Window: *window, DLConns: *dlConns, WebUI: *webui,
	})
	if err != nil {
		log.Fatalf("客户端启动失败: %v", err)
	}

	log.Printf("已连接服务端 %s", *server)
	log.Printf("本地入口 http://localhost%s（上传/下载流量由本机直连百度，不经过服务端）", *listen)
	if err := http.ListenAndServe(*listen, d.Handler()); err != nil {
		log.Fatalf("client: %v", err)
	}
}
