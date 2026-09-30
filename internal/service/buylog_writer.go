package service

// 本文件由原 mvc.go（1023 行大杂烩）按「分类」拆出，见 docs/架构说明.md

import (
	"bufio"
	"os"
)

func GetWriter(log string, minute string) {
	filePath := "..\\datas\\log\\buylog_" + minute + ".txt"

	file, err := os.OpenFile(filePath, os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		panic(err)
	}

	//及时关闭file句柄
	defer file.Close()
	//写入文件时，使用带缓存的 *Writer
	write := bufio.NewWriter(file)

	write.WriteString("\n")
	write.WriteString(log)

	//Flush将缓存的文件真正写入到文件中
	write.Flush()

}
