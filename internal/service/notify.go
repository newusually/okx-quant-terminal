package service

// 本文件由原 mvc.go（1023 行大杂烩）按「分类」拆出，见 docs/架构说明.md

import (
	"github.com/faiface/beep/speaker"
	"github.com/faiface/beep/wav"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

func PlayMusic() {
	// 打开MP3文件
	f, err := os.Open("brave heart.wav")
	if err != nil {
		panic(err)
	}

	// 创建MP3解码器
	streamer, format, err := wav.Decode(f)
	if err != nil {
		panic(err)
	}
	defer streamer.Close()

	// 初始化扬声器
	speaker.Init(format.SampleRate, format.SampleRate.N(time.Second/10))

	// 播放音频
	speaker.Play(streamer)

	// 等待音频播放完毕
	select {}
}

func SendDingMsg(msg string) {
	//请求地址模板
	webHook := `https://oapi.dingtalk.com/robot/send?access_token=f8195c9e4ad6da4427d67e80dffed5d07ecaca1d1e79462fb5c0a9c6b12e90f2`
	content := `{"msgtype": "text",
        "text": {"content": "` + msg + `"}
    }`
	//创建一个请求
	req, err := http.NewRequest("POST", webHook, strings.NewReader(content))
	if err != nil {
		// handle error
	}

	client := &http.Client{}
	//设置请求头
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	//发送请求
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[钉钉] 推送失败：%v", err)
		return
	}
	//关闭请求
	defer resp.Body.Close()
}
