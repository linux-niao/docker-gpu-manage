package computenode

import (
	"context"
	"time"

	"github.com/flipped-aurora/gin-vue-admin/server/global"
	model "github.com/flipped-aurora/gin-vue-admin/server/model/computenode"
	instanceSvc "github.com/flipped-aurora/gin-vue-admin/server/service/instance"
	"github.com/gogf/gf/v2/os/gcron"
	"go.uber.org/zap"
)

// StartDockerStatusCheckCron 启动Docker状态检查定时任务
// 每5分钟检查一次所有算力节点的 Docker 连接是否正常
func StartDockerStatusCheckCron() {
	// 每5分钟（在每个5分钟周期的第0秒触发）
	// 表达式：秒 分 时 日 月 周
	_, err := gcron.AddSingleton(context.Background(), "0 */5 * * * *", func(ctx context.Context) {
		checkAllNodeDockerStatus(ctx)
	}, "docker-status-check")
	if err != nil {
		global.GVA_LOG.Error("启动Docker状态检查定时任务失败", zap.Error(err))
		return
	}
	global.GVA_LOG.Info("Docker状态检查定时任务已启动，每5分钟检查一次")
}

// checkAllNodeDockerStatus 检查所有节点的 Docker 状态并更新数据库
func checkAllNodeDockerStatus(ctx context.Context) {
	var nodes []model.ComputeNode
	if err := global.GVA_DB.Where("deleted_at IS NULL").Find(&nodes).Error; err != nil {
		global.GVA_LOG.Error("查询算力节点失败", zap.Error(err))
		return
	}

	dockerSvc := instanceSvc.DockerService{}
	var success, failed int

	for i := range nodes {
		n := &nodes[i]

		// 为单个节点设置超时，避免长时间阻塞
		checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		connected, message := dockerSvc.TestDockerConnection(checkCtx, n)
		cancel()

		if connected {
			status := "connected"
			_ = global.GVA_DB.Model(n).Where("id = ?", n.ID).Update("docker_status", status).Error
			global.GVA_LOG.Debug("Docker连接正常", zap.Uint("nodeId", n.ID), zap.String("name", safeStr(n.Name)))
			success++
		} else {
			status := "failed"
			_ = global.GVA_DB.Model(n).Where("id = ?", n.ID).Update("docker_status", status).Error
			global.GVA_LOG.Warn("Docker连接异常", zap.Uint("nodeId", n.ID), zap.String("name", safeStr(n.Name)), zap.String("error", message))
			failed++
		}

		// 防止对远端Docker造成瞬时高压
		time.Sleep(100 * time.Millisecond)
	}

	global.GVA_LOG.Info("Docker状态检查完成", zap.Int("总节点", len(nodes)), zap.Int("正常", success), zap.Int("异常", failed))
}

func safeStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
