package api

import (
	"context"

	"ps2api/internal/provider"
)

// resolveVisionMessages 在图片识别开启时，逐条把消息 content 里的图片块识别成文本块（原地回写）。
// 识别失败返回 error，调用方应回退 400（绝不静默丢图）。未开启时直接返回 nil、不改动。
func (s *Server) resolveVisionMessages(ctx context.Context, msgs []provider.ChatMessage) error {
	if !s.Vision.Enabled() {
		return nil
	}
	for i := range msgs {
		resolved, changed, err := s.Vision.ResolveMedia(ctx, msgs[i].Content)
		if err != nil {
			return err
		}
		if changed {
			msgs[i].Content = resolved
		}
	}
	return nil
}
