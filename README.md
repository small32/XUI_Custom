# XUI_Custom

支持多协议多用户的 xray 面板

基于 [vaxilu/x-ui](https://github.com/vaxilu/x-ui) 定制，增加入站的 Clash 订阅长链接生成功能。

# 功能介绍

- 系统状态监控
- 支持多用户多协议，网页可视化操作
- 支持的协议：vmess、vless、trojan、shadowsocks、dokodemo-door、socks、http
- 支持配置更多传输配置
- 流量统计，限制流量，限制到期时间
- 可自定义 xray 配置模板
- 支持 https 访问面板（自备域名 + ssl 证书）
- 支持一键SSL证书申请且自动续签
- 入站操作菜单支持生成、查看和复制 Clash 定制订阅长链接
- 更多高级配置项，详见面板

# 生成订阅

在入站列表中，点击当前入站的“操作”→“生成订阅”，选择订阅方式后点击“复制订阅链接”，然后在对应客户端中添加订阅。

订阅采用进阶模式，可选择 Clash（Shadowrocket、Stash兼容）、Surge、Quantumult X 或 Loon，默认转换后端为 [https://api.wcc.best/sub?](https://api.wcc.best/sub?)。远程配置使用 ACL4SSR_Online_Mini 精简版（与 GitHub 同步）。

## 构建定制版本

本仓库已发布正式安装包，可按下面的安装与升级说明使用。需要自行编译时，从本仓库源码构建。上游发行包和第三方镜像不包含本仓库的定制修改。

```sh
git clone https://github.com/small32/XUI_Custom.git
cd XUI_Custom
go build -o x-ui main.go
```

也可在仓库根目录执行 `docker build -t x-ui-custom .` 构建定制镜像，再沿用原有部署方式。正式部署的 HTML 模板嵌入程序中，修改后需要重新构建。更新前备份原程序，保留现有数据库及配置。

# 安装&升级

安装和升级均使用 [本仓库最新正式发行包](https://github.com/small32/XUI_Custom/releases/latest)，提供 Linux amd64、arm64 安装包和 SHA256SUMS 校验文件。

## 一键安装与升级

```
bash <(curl -fLsS https://raw.githubusercontent.com/small32/XUI_Custom/main/install.sh)
```

## 手动安装&升级

1. 首先从 [本仓库 Releases](https://github.com/small32/XUI_Custom/releases) 下载本仓库发布的压缩包，一般选择 `amd64`架构；没有发行包时请从源码构建。
2. 然后将这个压缩包上传到服务器的 `/root/`目录下，并使用 `root`用户登录服务器

> 如果你的服务器 cpu 架构不是 `amd64`，自行将命令中的 `amd64`替换为其他架构

```
cd /root/
rm x-ui/ /usr/local/x-ui/ /usr/bin/x-ui -rf
tar zxvf x-ui-linux-amd64.tar.gz
chmod +x x-ui/x-ui x-ui/bin/xray-linux-* x-ui/x-ui.sh
cp x-ui/x-ui.sh /usr/bin/x-ui
cp -f x-ui/x-ui.service /etc/systemd/system/
mv x-ui/ /usr/local/
systemctl daemon-reload
systemctl enable x-ui
systemctl restart x-ui
```

## 使用docker安装

> 此 docker 教程与 docker 镜像由[Chasing66](https://github.com/Chasing66)提供

1. 安装docker

```shell
curl -fsSL https://get.docker.com | sh
```

2. 从本仓库构建并安装定制版

```shell
git clone https://github.com/small32/XUI_Custom.git
cd XUI_Custom
docker build -t x-ui-custom .
docker run -itd --network=host \
    -v $PWD/db/:/etc/x-ui/ \
    -v $PWD/cert/:/root/cert/ \
    --name x-ui --restart=unless-stopped \
    x-ui-custom
```

更新定制镜像时，在仓库目录拉取最新代码并重新构建，再使用原有数据库和证书挂载配置重新创建容器：

```shell
git pull --ff-only
docker build -t x-ui-custom .
```

## SSL证书申请

> 此功能与教程由[FranzKafkaYu](https://github.com/FranzKafkaYu)提供

脚本内置SSL证书申请功能，使用该脚本申请证书，需满足以下条件:

- 知晓Cloudflare 注册邮箱
- 知晓Cloudflare Global API Key
- 域名已通过cloudflare进行解析到当前服务器

获取Cloudflare Global API Key的方法:
    ![](media/bda84fbc2ede834deaba1c173a932223.png)
    ![](media/d13ffd6a73f938d1037d0708e31433bf.png)

使用时只需输入 `域名`, `邮箱`, `API KEY`即可，示意图如下：
        ![](media/2022-04-04_141259.png)

注意事项:

- 该脚本使用DNS API进行证书申请
- 默认使用Let'sEncrypt作为CA方
- 证书安装目录为/root/cert目录
- 本脚本申请证书均为泛域名证书

## 建议系统

- CentOS 7+
- Ubuntu 16+
- Debian 8+
