package main

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// tailFile 以 tail -f 模式读取文件，持续输出新增内容。
func tailFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "无法打开文件: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	// 定位到文件末尾
	if _, err := f.Seek(0, 2); err != nil {
		fmt.Fprintf(os.Stderr, "定位文件失败: %v\n", err)
		os.Exit(1)
	}

	// 捕获 Ctrl+C
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	done := false
	for !done {
		select {
		case <-sigCh:
			done = true
		default:
			if scanner.Scan() {
				fmt.Println(scanner.Text())
			} else {
				time.Sleep(200 * time.Millisecond)
			}
		}
	}
}
