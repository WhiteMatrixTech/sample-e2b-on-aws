# scripts/init-efs.sh
set -xe

# 安装 EFS 工具
yum install -y amazon-efs-utils

# 创建挂载目录
mkdir -p /mnt/efs

# 挂载 EFS 文件系统（变量由 Terraform 注入）
mount -t efs -o tls ${EFS_ID}:/ /mnt/efs

# 配置开机自动挂载
echo "${EFS_ID}:/ /mnt/efs efs _netdev,tls 0 0" >> /etc/fstab

echo "[init-efs] EFS 挂载完成"
