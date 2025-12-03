package instance

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/flipped-aurora/gin-vue-admin/server/global"
	"github.com/flipped-aurora/gin-vue-admin/server/model/computenode"
	"github.com/flipped-aurora/gin-vue-admin/server/model/imageregistry"
	instanceModel "github.com/flipped-aurora/gin-vue-admin/server/model/instance"
	"github.com/flipped-aurora/gin-vue-admin/server/model/product"
	"go.uber.org/zap"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

// DockerService Docker服务
type DockerService struct{}

// ContainerConfig 容器配置
type ContainerConfig struct {
	Image              string // 镜像地址
	Name               string // 容器名称
	CPUCores           int64  // CPU核心数
	MemoryGB           int64  // 内存大小(GB)
	SystemDiskGB       int64  // 系统盘大小(GB)
	DataDiskGB         int64  // 数据盘大小(GB)
	GPUCount           int64  // GPU数量
	SupportMemorySplit bool   // 是否支持显存分割
	MemoryCapacity     int64  // 显存容量(GB)
	PerCardCapacity    int64  // 节点单卡显存容量(GB)
}

// CreateDockerClient 创建Docker客户端
func (d *DockerService) CreateDockerClient(node *computenode.ComputeNode) (*client.Client, error) {
	if node.DockerAddress == nil || *node.DockerAddress == "" {
		return nil, fmt.Errorf("节点Docker连接地址为空")
	}

	dockerHost := *node.DockerAddress

	// 检查是否使用TLS
	useTLS := node.UseTls != nil && *node.UseTls

	if useTLS {
		// 使用TLS连接
		if node.CaCert == nil || node.ClientCert == nil || node.ClientKey == nil {
			return nil, fmt.Errorf("TLS证书配置不完整")
		}

		// 创建TLS配置
		tlsConfig, err := d.createTLSConfig(*node.CaCert, *node.ClientCert, *node.ClientKey)
		if err != nil {
			return nil, fmt.Errorf("创建TLS配置失败: %v", err)
		}

		// 创建HTTP客户端
		httpClient := &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: tlsConfig,
			},
			Timeout: 30 * time.Second,
		}

		return client.NewClientWithOpts(
			client.WithHost(dockerHost),
			client.WithHTTPClient(httpClient),
			client.WithAPIVersionNegotiation(),
		)
	}

	// 不使用TLS
	return client.NewClientWithOpts(
		client.WithHost(dockerHost),
		client.WithAPIVersionNegotiation(),
	)
}

// createTLSConfig 创建TLS配置
func (d *DockerService) createTLSConfig(caCert, clientCert, clientKey string) (*tls.Config, error) {
	// 加载CA证书
	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM([]byte(caCert)) {
		return nil, fmt.Errorf("无法解析CA证书")
	}

	// 加载客户端证书
	cert, err := tls.X509KeyPair([]byte(clientCert), []byte(clientKey))
	if err != nil {
		return nil, fmt.Errorf("加载客户端证书失败: %v", err)
	}

	return &tls.Config{
		RootCAs:      caCertPool,
		Certificates: []tls.Certificate{cert},
	}, nil
}

// CreateContainer 创建容器
func (d *DockerService) CreateContainer(ctx context.Context, node *computenode.ComputeNode, config *ContainerConfig) (containerID string, err error) {
	// 创建Docker客户端
	cli, err := d.CreateDockerClient(node)
	if err != nil {
		return "", fmt.Errorf("创建Docker客户端失败: %v", err)
	}
	defer cli.Close()

	// 构建容器配置
	containerConfig := &container.Config{
		Image: config.Image,
		Labels: map[string]string{
			"managed-by": "docker-gpu-manage",
			"instance":   config.Name,
		},
	}

	// 构建主机配置
	hostConfig := &container.HostConfig{}

	// CPU配置: --cpus=N
	if config.CPUCores > 0 {
		// NanoCPUs 是以纳秒为单位的CPU配额，1核 = 1e9 纳秒
		hostConfig.NanoCPUs = config.CPUCores * 1e9
	}

	// 内存配置: --memory=Ng
	if config.MemoryGB > 0 {
		// Memory 是以字节为单位
		hostConfig.Memory = config.MemoryGB * 1024 * 1024 * 1024
	}

	// 数据盘配置: 创建命名卷并挂载到 /data
	if config.DataDiskGB > 0 {
		volumeName := fmt.Sprintf("%s-data", config.Name)

		// 创建数据卷
		_, err = cli.VolumeCreate(ctx, volume.CreateOptions{
			Name: volumeName,
			Labels: map[string]string{
				"managed-by": "docker-gpu-manage",
				"instance":   config.Name,
			},
		})
		if err != nil {
			global.GVA_LOG.Warn("创建数据卷失败，可能已存在", zap.Error(err))
		}

		// 挂载数据卷到 /data
		hostConfig.Mounts = append(hostConfig.Mounts, mount.Mount{
			Type:   mount.TypeVolume,
			Source: volumeName,
			Target: "/data",
		})
	}

	// 显存分割配置: 如果支持显存分割，添加相关卷挂载和环境变量
	if config.SupportMemorySplit && config.MemoryCapacity > 0 && config.PerCardCapacity > 0 {
		// 挂载 HAMi 库目录: -v /root/hequan/HAMi-core-main/build:/libvgpu/build
		hostConfig.Mounts = append(hostConfig.Mounts, mount.Mount{
			Type:   mount.TypeBind,
			Source: "/root/hequan/HAMi-core-main/build",
			Target: "/libvgpu/build",
		})

		// 初始化环境变量（如果还没有）
		if containerConfig.Env == nil {
			containerConfig.Env = []string{}
		}

		// 添加环境变量: LD_PRELOAD=/libvgpu/build/libvgpu.so
		containerConfig.Env = append(containerConfig.Env, "LD_PRELOAD=/libvgpu/build/libvgpu.so")

		// 添加环境变量: CUDA_DEVICE_MEMORY_LIMIT=产品规格中的显存容量 g
		containerConfig.Env = append(containerConfig.Env, fmt.Sprintf("CUDA_DEVICE_MEMORY_LIMIT=%dg", config.MemoryCapacity))

		// 计算 CUDA_DEVICE_SM_LIMIT: 产品规格中的显存容量 / 主机本来的单卡总容量（整数）
		smLimit := int64(0)
		if config.PerCardCapacity > 0 {
			smLimit = config.MemoryCapacity / config.PerCardCapacity
		}
		// 添加环境变量: CUDA_DEVICE_SM_LIMIT
		containerConfig.Env = append(containerConfig.Env, fmt.Sprintf("CUDA_DEVICE_SM_LIMIT=%d", smLimit))

		global.GVA_LOG.Info("添加显存分割配置",
			zap.String("容器名称", config.Name),
			zap.Int64("显存容量", config.MemoryCapacity),
			zap.Int64("单卡容量", config.PerCardCapacity),
			zap.Int64("SM限制", smLimit))
	}

	// GPU配置: --gpus N
	if config.GPUCount > 0 {
		hostConfig.DeviceRequests = []container.DeviceRequest{
			{
				Driver: "nvidia",
				Count:  int(config.GPUCount),
				Capabilities: [][]string{
					{"gpu"},
				},
			},
		}
	}

	// 系统盘配置: --storage-opt overlay2.size=NG
	// 如果配置了系统盘大小，尝试添加存储选项
	if config.SystemDiskGB > 0 {
		hostConfig.StorageOpt = map[string]string{
			"overlay2.size": fmt.Sprintf("%dG", config.SystemDiskGB),
		}
	}

	// 尝试创建容器（先尝试带系统盘限制）
	var resp container.CreateResponse
	resp, err = cli.ContainerCreate(ctx, containerConfig, hostConfig, nil, nil, config.Name)

	// 如果创建失败且设置了系统盘参数，尝试不带系统盘参数重试
	if err != nil && config.SystemDiskGB > 0 {
		global.GVA_LOG.Warn("使用系统盘参数创建容器失败，尝试不带系统盘参数重试",
			zap.Error(err),
			zap.Int64("systemDiskGB", config.SystemDiskGB))

		// 清除系统盘参数
		hostConfig.StorageOpt = nil

		// 重试创建容器
		resp, err = cli.ContainerCreate(ctx, containerConfig, hostConfig, nil, nil, config.Name)
	}

	if err != nil {
		return "", fmt.Errorf("创建容器失败: %v", err)
	}

	// 启动容器
	if err = cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		// 启动失败，删除已创建的容器
		_ = cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return "", fmt.Errorf("启动容器失败: %v", err)
	}

	return resp.ID, nil
}

// DeleteContainer 删除容器及其数据卷
func (d *DockerService) DeleteContainer(ctx context.Context, node *computenode.ComputeNode, containerID string, containerName string) error {
	cli, err := d.CreateDockerClient(node)
	if err != nil {
		return fmt.Errorf("创建Docker客户端失败: %v", err)
	}
	defer cli.Close()

	// 先获取容器信息，提取所有挂载的命名卷
	var volumeNames []string
	inspect, err := cli.ContainerInspect(ctx, containerID)
	if err == nil {
		// 从容器挂载信息中提取所有命名卷
		for _, m := range inspect.Mounts {
			if m.Type == mount.TypeVolume && m.Name != "" {
				volumeNames = append(volumeNames, m.Name)
			}
		}
	} else {
		// 如果无法获取容器信息，尝试使用默认的命名规则
		global.GVA_LOG.Warn("获取容器信息失败，使用默认卷名", zap.Error(err))
		if containerName != "" {
			volumeNames = append(volumeNames, fmt.Sprintf("%s-data", containerName))
		}
	}

	// 先停止容器
	timeout := 10
	_ = cli.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &timeout})

	// 删除容器
	err = cli.ContainerRemove(ctx, containerID, container.RemoveOptions{
		Force:         true,
		RemoveVolumes: true, // 同时删除匿名卷
	})
	if err != nil {
		global.GVA_LOG.Warn("删除容器失败", zap.Error(err))
		// 即使容器删除失败，也继续删除数据卷
	}

	// 删除所有挂载的命名数据卷
	for _, volumeName := range volumeNames {
		err = cli.VolumeRemove(ctx, volumeName, true)
		if err != nil {
			// 记录警告但不中断流程，因为卷可能已经被删除或不存在
			global.GVA_LOG.Warn("删除数据卷失败", zap.String("volume", volumeName), zap.Error(err))
		} else {
			global.GVA_LOG.Info("成功删除数据卷", zap.String("volume", volumeName))
		}
	}

	return nil
}

// StopContainer 停止容器
func (d *DockerService) StopContainer(ctx context.Context, node *computenode.ComputeNode, containerID string) error {
	cli, err := d.CreateDockerClient(node)
	if err != nil {
		return fmt.Errorf("创建Docker客户端失败: %v", err)
	}
	defer cli.Close()

	timeout := 30
	return cli.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &timeout})
}

// StartContainer 启动容器
func (d *DockerService) StartContainer(ctx context.Context, node *computenode.ComputeNode, containerID string) error {
	cli, err := d.CreateDockerClient(node)
	if err != nil {
		return fmt.Errorf("创建Docker客户端失败: %v", err)
	}
	defer cli.Close()

	return cli.ContainerStart(ctx, containerID, container.StartOptions{})
}

// RestartContainer 重启容器
func (d *DockerService) RestartContainer(ctx context.Context, node *computenode.ComputeNode, containerID string) error {
	cli, err := d.CreateDockerClient(node)
	if err != nil {
		return fmt.Errorf("创建Docker客户端失败: %v", err)
	}
	defer cli.Close()

	timeout := 30
	return cli.ContainerRestart(ctx, containerID, container.StopOptions{Timeout: &timeout})
}

// GetContainerStatus 获取容器状态
func (d *DockerService) GetContainerStatus(ctx context.Context, node *computenode.ComputeNode, containerID string) (string, error) {
	cli, err := d.CreateDockerClient(node)
	if err != nil {
		return "", fmt.Errorf("创建Docker客户端失败: %v", err)
	}
	defer cli.Close()

	inspect, err := cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", fmt.Errorf("获取容器信息失败: %v", err)
	}

	return inspect.State.Status, nil
}

// BuildContainerConfig 根据实例信息构建容器配置
func (d *DockerService) BuildContainerConfig(image *imageregistry.ImageRegistry, spec *product.ProductSpec, node *computenode.ComputeNode, instanceName string) *ContainerConfig {
	config := &ContainerConfig{
		Name: instanceName,
	}

	// 镜像地址
	if image.Address != nil {
		config.Image = *image.Address
	}

	// CPU核心数
	if spec.CpuCores != nil {
		config.CPUCores = *spec.CpuCores
	}

	// 内存大小
	if spec.MemoryGb != nil {
		config.MemoryGB = *spec.MemoryGb
	}

	// 系统盘大小
	if spec.SystemDiskGb != nil {
		config.SystemDiskGB = *spec.SystemDiskGb
	}

	// 数据盘大小
	if spec.DataDiskGb != nil {
		config.DataDiskGB = *spec.DataDiskGb
	}

	// GPU数量
	if spec.GpuCount != nil {
		config.GPUCount = *spec.GpuCount
	}

	// 显存分割相关配置
	if spec.SupportMemorySplit != nil && *spec.SupportMemorySplit {
		config.SupportMemorySplit = true
		if spec.MemoryCapacity != nil {
			config.MemoryCapacity = *spec.MemoryCapacity
		}
		// 计算节点单卡显存容量
		if node.MemoryCapacity != nil && node.GpuCount != nil && *node.GpuCount > 0 {
			config.PerCardCapacity = *node.MemoryCapacity / *node.GpuCount
		}
	}

	return config
}

// GenerateInstanceName 生成实例名称
func (d *DockerService) GenerateInstanceName(baseName string, instanceID uint) string {
	// 清理名称，只保留字母数字和横杠
	cleanName := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '-'
	}, baseName)

	// 移除连续的横杠
	for strings.Contains(cleanName, "--") {
		cleanName = strings.ReplaceAll(cleanName, "--", "-")
	}

	// 移除首尾横杠
	cleanName = strings.Trim(cleanName, "-")

	if cleanName == "" {
		cleanName = "instance"
	}

	return fmt.Sprintf("%s-%d-%d", cleanName, instanceID, time.Now().Unix())
}

// SyncContainerStatus 同步容器状态到数据库
func (d *DockerService) SyncContainerStatus(ctx context.Context, instanceID uint) error {
	var inst instanceModel.Instance
	if err := global.GVA_DB.Where("id = ?", instanceID).First(&inst).Error; err != nil {
		return err
	}

	if inst.ContainerId == nil || *inst.ContainerId == "" || inst.NodeId == nil {
		return nil
	}

	var node computenode.ComputeNode
	if err := global.GVA_DB.Where("id = ?", *inst.NodeId).First(&node).Error; err != nil {
		return err
	}

	status, err := d.GetContainerStatus(ctx, &node, *inst.ContainerId)
	if err != nil {
		status = "unknown"
	}

	return global.GVA_DB.Model(&inst).Update("container_status", status).Error
}

// GetContainerLogs 获取容器日志
func (d *DockerService) GetContainerLogs(ctx context.Context, node *computenode.ComputeNode, containerID string, tail string) (string, error) {
	cli, err := d.CreateDockerClient(node)
	if err != nil {
		return "", fmt.Errorf("创建Docker客户端失败: %v", err)
	}
	defer cli.Close()

	options := container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       tail,
		Timestamps: true,
	}

	logs, err := cli.ContainerLogs(ctx, containerID, options)
	if err != nil {
		return "", fmt.Errorf("获取容器日志失败: %v", err)
	}
	defer logs.Close()

	// 读取日志内容
	buf := new(bytes.Buffer)
	_, err = buf.ReadFrom(logs)
	if err != nil {
		return "", fmt.Errorf("读取日志内容失败: %v", err)
	}

	return buf.String(), nil
}

// ptr 辅助函数：创建字符串指针
func ptr(s string) *string {
	return &s
}

// int64Ptr 辅助函数：创建int64指针
func int64Ptr(i int64) *int64 {
	return &i
}

// parseInt64 解析字符串为int64
func parseInt64(s string) int64 {
	i, _ := strconv.ParseInt(s, 10, 64)
	return i
}

// TestDockerConnection 测试Docker连接
func (d *DockerService) TestDockerConnection(ctx context.Context, node *computenode.ComputeNode) (bool, string) {
	if node.DockerAddress == nil || *node.DockerAddress == "" {
		return false, "Docker连接地址为空"
	}

	cli, err := d.CreateDockerClient(node)
	if err != nil {
		return false, fmt.Sprintf("创建Docker客户端失败: %v", err)
	}
	defer cli.Close()

	// 尝试ping Docker服务
	_, err = cli.Ping(ctx)
	if err != nil {
		return false, fmt.Sprintf("Docker连接失败: %v", err)
	}

	return true, "连接成功"
}

// ContainerStats 容器统计信息
type ContainerStats struct {
	CPUUsagePercent    float64 `json:"cpuUsagePercent"`    // CPU使用率百分比
	MemoryUsage        int64   `json:"memoryUsage"`        // 内存使用量（字节）
	MemoryLimit        int64   `json:"memoryLimit"`        // 内存限制（字节）
	MemoryUsagePercent float64 `json:"memoryUsagePercent"` // 内存使用率百分比
	NetworkRx          int64   `json:"networkRx"`          // 网络接收字节数
	NetworkTx          int64   `json:"networkTx"`          // 网络发送字节数
	BlockRead          int64   `json:"blockRead"`          // 块设备读取字节数
	BlockWrite         int64   `json:"blockWrite"`         // 块设备写入字节数
	Pids               uint64  `json:"pids"`               // 进程数
}

// GetContainerStats 获取容器统计信息
func (d *DockerService) GetContainerStats(ctx context.Context, node *computenode.ComputeNode, containerID string) (*ContainerStats, error) {
	cli, err := d.CreateDockerClient(node)
	if err != nil {
		return nil, fmt.Errorf("创建Docker客户端失败: %v", err)
	}
	defer cli.Close()

	// 获取容器统计信息
	stats, err := cli.ContainerStats(ctx, containerID, false)
	if err != nil {
		return nil, fmt.Errorf("获取容器统计信息失败: %v", err)
	}
	defer stats.Body.Close()

	// 解析统计信息
	var v types.StatsJSON
	if err := json.NewDecoder(stats.Body).Decode(&v); err != nil {
		return nil, fmt.Errorf("解析统计信息失败: %v", err)
	}

	// 计算CPU使用率
	var cpuPercent float64
	if v.CPUStats.CPUUsage.TotalUsage > 0 && v.PreCPUStats.CPUUsage.TotalUsage > 0 {
		cpuDelta := float64(v.CPUStats.CPUUsage.TotalUsage - v.PreCPUStats.CPUUsage.TotalUsage)
		systemDelta := float64(v.CPUStats.SystemUsage - v.PreCPUStats.SystemUsage)
		if systemDelta > 0 {
			cpuPercent = (cpuDelta / systemDelta) * float64(len(v.CPUStats.CPUUsage.PercpuUsage)) * 100.0
		}
	}

	// 计算内存使用率
	var memoryPercent float64
	memoryUsage := v.MemoryStats.Usage
	memoryLimit := v.MemoryStats.Limit
	if memoryLimit > 0 {
		memoryPercent = float64(memoryUsage) / float64(memoryLimit) * 100.0
	}

	// 获取网络统计
	var networkRx, networkTx int64
	if len(v.Networks) > 0 {
		for _, network := range v.Networks {
			networkRx += int64(network.RxBytes)
			networkTx += int64(network.TxBytes)
		}
	}

	// 获取块设备统计
	var blockRead, blockWrite int64
	if len(v.BlkioStats.IoServiceBytesRecursive) > 0 {
		for _, entry := range v.BlkioStats.IoServiceBytesRecursive {
			if entry.Op == "Read" {
				blockRead += int64(entry.Value)
			} else if entry.Op == "Write" {
				blockWrite += int64(entry.Value)
			}
		}
	}

	// 获取进程数
	pids := v.PidsStats.Current

	return &ContainerStats{
		CPUUsagePercent:    cpuPercent,
		MemoryUsage:        int64(memoryUsage),
		MemoryLimit:        int64(memoryLimit),
		MemoryUsagePercent: memoryPercent,
		NetworkRx:          networkRx,
		NetworkTx:          networkTx,
		BlockRead:          blockRead,
		BlockWrite:         blockWrite,
		Pids:               pids,
	}, nil
}
