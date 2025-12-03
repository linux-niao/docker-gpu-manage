package instance

import (
	"context"
	"time"

	"github.com/flipped-aurora/gin-vue-admin/server/global"
	instanceModel "github.com/flipped-aurora/gin-vue-admin/server/model/instance"
	"github.com/gogf/gf/v2/os/gcron"
	"go.uber.org/zap"
)

// StartContainerStatusCheckCron 启动容器状态检查定时任务
// 每30秒检查一次所有容器的运行状态
func StartContainerStatusCheckCron() {
	// 使用 gcron 创建定时任务，每30秒执行一次
	// cron 表达式格式：秒 分钟 小时 日 月 星期
	_, err := gcron.AddSingleton(context.Background(), "*/30 * * * * *", func(ctx context.Context) {
		checkAllContainerStatus(ctx)
	}, "container-status-check")
	if err != nil {
		global.GVA_LOG.Error("启动容器状态检查定时任务失败", zap.Error(err))
		return
	}
	global.GVA_LOG.Info("容器状态检查定时任务已启动，每30秒检查一次")
}

// checkAllContainerStatus 检查所有容器的状态
func checkAllContainerStatus(ctx context.Context) {

	// 获取所有有容器ID的实例（未删除的）
	var instances []instanceModel.Instance
	if err := global.GVA_DB.Where("deleted_at IS NULL AND container_id IS NOT NULL AND container_id != ''").Find(&instances).Error; err != nil {
		global.GVA_LOG.Error("查询实例列表失败", zap.Error(err))
		return
	}

	total := len(instances)
	successCount := 0
	failCount := 0

	// 遍历每个实例，检查容器状态
	for _, inst := range instances {
		if inst.ContainerId == nil || *inst.ContainerId == "" || inst.NodeId == nil {
			continue
		}

		// 使用已有的同步状态方法
		if err := dockerService.SyncContainerStatus(ctx, inst.ID); err != nil {
			global.GVA_LOG.Error("同步容器状态失败",
				zap.Uint("实例ID", inst.ID),
				zap.String("容器ID", *inst.ContainerId),
				zap.Error(err))
			failCount++
		} else {
			successCount++
		}

		// 添加小延迟，避免对Docker API造成过大压力
		time.Sleep(100 * time.Millisecond)
	}

	global.GVA_LOG.Info("容器状态检查完成",
		zap.Int("总数", total),
		zap.Int("成功", successCount),
		zap.Int("失败", failCount))
}
