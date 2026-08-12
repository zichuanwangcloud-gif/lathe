package linear

import (
	"context"
	"fmt"
	"strings"
)

// Viewer 是当前令牌对应的 Linear 账号。
type Viewer struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

const viewerQuery = `query { viewer { id name email } }`

// Verify 校验令牌是否可用，并返回对应账号。
//
// 返回的 ID 正是「只接指派给我的 issue」所需的用户标识（D2），
// 因此配置凭据时顺带就拿到了，不必再让人手工去 Linear 里翻。
func (c *Client) Verify(ctx context.Context) (*Viewer, error) {
	var resp struct {
		Viewer *Viewer `json:"viewer"`
	}
	if err := c.do(ctx, viewerQuery, nil, &resp); err != nil {
		return nil, classifyVerifyError(err)
	}
	if resp.Viewer == nil || resp.Viewer.ID == "" {
		return nil, fmt.Errorf("linear: 令牌有效但未取到账号信息，请确认这是个人 API key")
	}
	return resp.Viewer, nil
}

// classifyVerifyError 把底层错误翻译成可操作的提示。
//
// 「令牌不对」与「网络不通」需要完全不同的处理，笼统报「验证失败」
// 会让人白折腾。
func classifyVerifyError(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "Authentication") ||
		strings.Contains(msg, "authentication") ||
		strings.Contains(msg, "HTTP 401") ||
		strings.Contains(msg, "HTTP 403"):
		return fmt.Errorf("令牌无效或已过期，请在 Linear → Settings → Security & access → Personal API keys 重新签发")
	case strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "deadline exceeded"):
		return fmt.Errorf("连不上 Linear（网络或代理问题）：%w", err)
	default:
		return err
	}
}
