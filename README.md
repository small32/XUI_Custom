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

# 生成 Clash 订阅

入站列表 → 当前入站的“操作” → “生成Clash订阅” → “复制订阅链接”，然后在 Clash 客户端中添加订阅。

生成规则与 [ACL4SSR 在线订阅转换](https://acl4ssr-sub.github.io/) 的以下设置一致（其余参数采用 2026-09-16 网站初始默认值）：

| 设置 | 值 |
| --- | --- |
| 模式 | 进阶模式 |
| 客户端 | Clash |
| 后端 | 默认：`https://api.wcc.best/sub?` |
| 远程配置 | ACL4SSR_Mini 本地 精简版：`config/ACL4SSR_Mini.ini` |
| 输出 | “定制订阅”长链接，不生成短链接 |

菜单仅对支持生成节点链接的入站显示，与二维码使用同一份节点信息。每次生成包含当前入站，不汇总其他入站；转换后端和客户端是否支持具体协议及传输配置，以实际导入结果为准。

链接在浏览器本地生成；客户端导入或更新订阅时，会把节点信息发送给 `api.wcc.best`。长链接包含节点凭据，请妥善保管。账号、密码、地址或端口等配置变更后，需要重新生成并替换客户端中的订阅链接。

## 构建定制版本

此功能需要从本仓库源码构建。下面的上游安装脚本、上游发行包和第三方镜像不包含本仓库的定制修改。

```sh
git clone https://github.com/small32/XUI_Custom.git
cd XUI_Custom
go build -o x-ui main.go
```

也可在仓库根目录执行 `docker build -t x-ui-custom .` 构建定制镜像，再沿用原有部署方式。正式部署的 HTML 模板嵌入程序中，修改后需要重新构建。更新前备份原程序，保留现有数据库及配置。

# 安装&升级

```
bash <(curl -Ls https://raw.githubusercontent.com/vaxilu/x-ui/master/install.sh)
```

## 手动安装&升级

1. 首先从 https://github.com/vaxilu/x-ui/releases 下载最新的压缩包，一般选择 `amd64`架构
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

2. 安装x-ui

```shell
mkdir x-ui && cd x-ui
docker run -itd --network=host \
    -v $PWD/db/:/etc/x-ui/ \
    -v $PWD/cert/:/root/cert/ \
    --name x-ui --restart=unless-stopped \
    enwaiax/x-ui:latest
```

> Build 自己的镜像

```shell
docker build -t x-ui .
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

## Tg机器人使用（开发中，暂不可使用）

> 此功能与教程由[FranzKafkaYu](https://github.com/FranzKafkaYu)提供

X-UI支持通过Tg机器人实现每日流量通知，面板登录提醒等功能，使用Tg机器人，需要自行申请
具体申请教程可以参考[博客链接](https://coderfan.net/how-to-use-telegram-bot-to-alarm-you-when-someone-login-into-your-vps.html)
使用说明:在面板后台设置机器人相关参数，具体包括

- Tg机器人Token
- Tg机器人ChatId
- Tg机器人周期运行时间，采用crontab语法  

参考语法：
- 30 * * * * * //每一分的第30s进行通知
- @hourly      //每小时通知
- @daily       //每天通知（凌晨零点整）
- @every 8h    //每8小时通知  

TG通知内容：
- 节点流量使用
- 面板登录提醒
- 节点到期提醒
- 流量预警提醒  

更多功能规划中...
## 建议系统

- CentOS 7+
- Ubuntu 16+
- Debian 8+

# 常见问题

## 从 v2-ui 迁移

首先在安装了 v2-ui 的服务器上安装最新版 x-ui，然后使用以下命令进行迁移，将迁移本机 v2-ui 的 `所有 inbound 账号数据`至 x-ui，`面板设置和用户名密码不会迁移`

> 迁移成功后请 `关闭 v2-ui`并且 `重启 x-ui`，否则 v2-ui 的 inbound 会与 x-ui 的 inbound 会产生 `端口冲突`

```
x-ui v2-ui
```

## issue 关闭

各种小白问题看得血压很高

## Stargazers over time

[![Stargazers over time](https://starchart.cc/vaxilu/x-ui.svg)](https://starchart.cc/vaxilu/x-ui)
