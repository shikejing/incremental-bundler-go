package bundler

import "context"

// runPipeline 按固定顺序对单个模块执行转换流水线。
// 任一步骤失败即中止该模块，返回可定位到模块与步骤的 TransformError；
// 失败模块的半成品内容不会返回给调用方。
func runPipeline(ctx context.Context, transforms []Transform, moduleID string, src []byte) ([]byte, error) {
	cur := src
	for _, t := range transforms {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out, err := t.Apply(ctx, moduleID, cur)
		if err != nil {
			return nil, &TransformError{Module: moduleID, Step: t.Name(), Err: err}
		}
		cur = out
	}
	return cur, nil
}
