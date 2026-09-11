package mail

import (
	"context"
	"errors"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/Clouditera/lathe/internal/store"
)

// silentSMTP 起一个「能建立 TCP 连接但从不吐一个字节」的假服务器。
//
// 这正是让发信永久挂住的那种对端：防火墙对已建连接静默 DROP、SMTP 假死、
// 负载均衡把包吞掉。TCP connect 秒过，所以 net.Dialer.Timeout 完全帮不上忙；
// 之后 smtp.NewClient 会同步等 220 欢迎语，那次读没有 deadline 就是等一辈子。
func silentSMTP(t *testing.T) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起假 SMTP 失败: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// 刻意什么都不写、也不关：连接就这么吊着。
			t.Cleanup(func() { _ = c.Close() })
		}
	}()

	h, p, _ := net.SplitHostPort(ln.Addr().String())
	n, _ := strconv.Atoi(p)
	return h, n
}

// TestSendReturnsOnSilentServer 钉住会话级 IO deadline。
//
// 没有 deadline 时 Send 永不返回，这条测试会被 go test 的超时杀掉（panic:
// test timed out）而不是失败——所以断言写成「必须在 sessionTimeout 之后、
// 但远早于任何合理的测试超时之前返回」。
func TestSendReturnsOnSilentServer(t *testing.T) {
	if os.Getenv("LATHE_SKIP_SLOW") != "" {
		t.Skip("跳过慢测试（LATHE_SKIP_SLOW）")
	}
	host, port := silentSMTP(t)

	s := NewSender(func(ctx context.Context) (store.SMTPConfig, string, error) {
		return store.SMTPConfig{
			Host: host, Port: port,
			FromAddr: "lathe@example.com", FromName: "Lathe",
			TLSMode: store.TLSNone,
		}, "", nil
	})

	// 上界给得比 sessionTimeout 宽裕，但远小于「永久挂住」。
	const budget = 2 * time.Minute

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- s.Send(context.Background(), "owner@example.com", "主题", "正文") }()

	select {
	case err := <-done:
		elapsed := time.Since(start)
		if err == nil {
			t.Fatalf("对着一个从不应答的服务器发信竟然成功了（耗时 %v）", elapsed)
		}
		// 必须是超时类错误，不能是「立刻拒绝」——后者说明根本没连上，
		// 那这条测试就没在测它该测的东西。
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("期望超时错误，得到 %T: %v（耗时 %v）", err, err, elapsed)
		}
		if elapsed < 5*time.Second {
			t.Fatalf("返回得太快（%v），假死场景下不该这么早——测试可能没真连上", elapsed)
		}
		t.Logf("假死服务器上 Send 在 %v 后以超时收口", elapsed)
	case <-time.After(budget):
		t.Fatalf("Send 在 %v 内没有返回：会话级 IO deadline 失效，发信可被永久挂住", budget)
	}
}

// TestSendUnblocksOnContextCancel 钉住 watchContext：调用方取消时不必等满
// sessionTimeout。net/smtp 不感知 context，唯一能打断阻塞读的手段就是把 conn 关掉。
func TestSendUnblocksOnContextCancel(t *testing.T) {
	host, port := silentSMTP(t)

	s := NewSender(func(ctx context.Context) (store.SMTPConfig, string, error) {
		return store.SMTPConfig{
			Host: host, Port: port,
			FromAddr: "lathe@example.com", FromName: "Lathe",
			TLSMode: store.TLSNone,
		}, "", nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- s.Send(ctx, "owner@example.com", "主题", "正文") }()

	// 给它时间真正连上并卡在等欢迎语上，然后取消。
	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("取消之后 Send 竟然报成功")
		}
		if elapsed > 10*time.Second {
			t.Fatalf("取消后 %v 才返回：说明是 sessionTimeout 兜住的，watchContext 没起作用", elapsed)
		}
		t.Logf("取消后 %v 返回：%v", elapsed, err)
	case <-time.After(30 * time.Second):
		t.Fatal("取消后 Send 仍未返回：watchContext 失效")
	}
}
