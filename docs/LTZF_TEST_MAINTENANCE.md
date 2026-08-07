# 蓝兔支付测试环境维护方案

## 1. 当前环境

| 项目 | 值 |
| --- | --- |
| 服务器 | `123.56.186.169` |
| SSH 用户 | `root` |
| SSH 端口 | `22` |
| 生产域名 | `https://ai.akaii.top/` |
| 测试域名 | `https://test.lingyigewo.me/` |
| 蓝兔定制分支 | `custom/ltzf` |
| 官方上游分支 | `upstream/main` |

SSH 密码不写入仓库、文档、脚本或命令行参数。连接服务器时手工输入，或使用服务器上的 SSH 密钥。

## 2. 关键隔离原则

1. 生产域名 `ai.akaii.top` 和测试域名 `test.lingyigewo.me` 必须走不同的反向代理路由。
2. 测试容器只绑定 `127.0.0.1` 的独立端口，例如 `18080:8080`，不能抢占生产的 80、443 或应用端口。
3. 测试使用独立 Compose 项目名，例如 `sub2api-ltzf-test`，不能直接操作生产项目。
4. 禁止执行 `docker compose down -v`、`docker volume rm`、`git reset --hard` 等可能破坏生产数据或定制代码的命令。
5. 本次蓝兔代码没有数据库 schema、migration 或 SQL 改动，但管理员保存支付配置会写入现有支付配置表。

## 3. 数据库共享边界

“没有改数据库结构”不等于“共享数据库没有风险”。如果测试环境和生产环境连接同一个正在使用的数据库：

- 测试环境新增或修改蓝兔服务商配置，生产环境也会看到；
- 支付方式启用、来源路由、限额等配置是全局数据，可能影响生产用户；
- 测试订单、回调和充值数据会进入生产数据；
- 两套实例共用 Redis 时，缓存键、定时任务和锁也可能互相影响。

本次测试服务器采用独立数据栈：

- 测试使用独立 PostgreSQL 容器和 `postgres_data` 目录；
- 测试使用独立 Redis 容器和 `redis_data` 目录；
- 不连接生产数据库，不连接生产 Redis，不执行生产数据操作。

如果以后需要带入现有测试账号，应先从生产导出一次性备份，在测试服务器恢复到独立数据库；不要把测试实例直接接入生产数据库。

如果必须实时共享业务数据，必须接受“支付配置和测试订单会影响生产”的结果，并且测试期间不要启用生产可见的蓝兔支付来源。这个限制不是 Git 或 Docker 能解决的。

## 4. Git 分支维护模型

仓库关系：

```text
官方仓库 Wei-Shaw/sub2api: main
             |
             v
本 fork: Typedef-jiak/sub2api: custom/ltzf
             |
             v
服务器只部署 origin/custom/ltzf
```

首次在服务器准备代码：

```bash
mkdir -p /opt
cd /opt
git clone -b custom/ltzf https://github.com/Typedef-jiak/sub2api.git sub2api-ltzf-test
cd /opt/sub2api-ltzf-test
git remote add upstream https://github.com/Wei-Shaw/sub2api.git
```

服务器以后只拉取定制分支：

```bash
cd /opt/sub2api-ltzf-test
git switch custom/ltzf
git pull --ff-only origin custom/ltzf
```

同步官方更新时，在维护机器上执行：

```bash
git switch custom/ltzf
git fetch upstream
git merge --no-edit upstream/main
```

如果出现冲突，手工解决、运行测试后再推送：

```bash
git add <已解决的文件>
git commit
git push origin custom/ltzf
```

不要在定制分支执行 `git reset --hard upstream/main`，否则会丢失蓝兔提交。官方更新与蓝兔改动冲突时，Git 应该停下来让人处理，而不是静默覆盖。

## 5. 服务器测试部署

### 5.1 本地构建并上传测试镜像

在本地工作区构建，避免测试服务器消耗编译资源：

```powershell
cd D:\sub2api
git switch custom/ltzf
git pull --ff-only origin custom/ltzf

$TAG = "ltzf-$(git rev-parse --short=8 HEAD)"
docker build -t "sub2api:$TAG" `
  --build-arg GOPROXY=https://goproxy.cn,direct `
  --build-arg GOSUMDB=sum.golang.google.cn `
  --build-arg NPM_CONFIG_REGISTRY=https://registry.npmmirror.com .
docker save -o ".\sub2api-$TAG.tar" "sub2api:$TAG"
```

通过 SSH/SFTP 将镜像包上传到测试服务器 `/opt/sub2api-ltzf-test/`，然后在服务器加载：

```bash
cd /opt/sub2api-ltzf-test
docker load -i sub2api-ltzf-c11dc55d.tar
```

### 5.2 测试 Compose

服务器当前部署目录为 `/opt/sub2api-ltzf-test`，使用独立项目启动：

```bash
cd /opt/sub2api-ltzf-test
docker compose up -d
```

查看状态和日志：

```bash
docker compose ps
docker compose logs -f app
```

### 5.3 HTTPS 反向代理

1. 将 `test.lingyigewo.me` 的 DNS A 记录指向 `123.56.186.169`（当前通过 Cloudflare 代理）。
2. 测试服务器的 Caddy 容器独占 80/443，并只匹配 `test.lingyigewo.me`。
3. Caddy 通过 Compose 内部网络转发到 `app:8080`，应用宿主机端口仅绑定 `127.0.0.1:18080`。
4. Caddy 自动签发和续期 HTTPS 证书。
5. 不要在生产服务器增加代理或修改 `ai.akaii.top` 的路由。

验证：

```bash
curl -I https://test.lingyigewo.me/
curl -I https://ai.akaii.top/
```

两条域名都必须返回预期结果，生产域名不能因为测试部署而改变。

## 6. 更新测试环境

```powershell
cd D:\sub2api
git pull --ff-only origin custom/ltzf
$TAG = "ltzf-$(git rev-parse --short=8 HEAD)"
docker build -t "sub2api:$TAG" `
  --build-arg GOPROXY=https://goproxy.cn,direct `
  --build-arg GOSUMDB=sum.golang.google.cn `
  --build-arg NPM_CONFIG_REGISTRY=https://registry.npmmirror.com .
docker save -o ".\sub2api-$TAG.tar" "sub2api:$TAG"
# 上传镜像包到服务器后执行：
# docker load -i /opt/sub2api-ltzf-test/sub2api-$TAG.tar
```

服务器上更新镜像标签后执行 `docker compose up -d`；保留最近几个镜像标签，出现问题时把 Compose 的 `image` 改回上一个已验证的提交再执行 `up -d`。

## 7. 蓝兔支付测试检查表

- 管理员登录测试环境后台，确认普通用户无法访问 `/admin/settings`。
- 只在测试环境配置蓝兔商户号、商户密钥和回调地址。
- 桌面端验证 Native 二维码支付。
- 手机端验证 H5 跳转支付。
- 验证蓝兔回调地址为：`https://test.lingyigewo.me/api/v1/payment/webhook/ltzf`。
- 验证回调成功响应为纯文本大写 `SUCCESS`。
- 检查生产环境 `ai.akaii.top` 的支付方式、订单和日志没有被测试请求改变。
- 测试完成后，禁用测试环境蓝兔服务商实例或停止测试 Compose 项目。

## 8. 当前实现状态

- 蓝兔代码已推送到 `origin/custom/ltzf`。
- 最新提交：`44280eba`。
- 当前提交只包含 `backend/` 和 `frontend/` 文件。
- 没有新增数据库表、字段、索引或迁移文件。
- 当前分支没有发布到 GHCR/Docker Hub；镜像由本地构建后通过 SFTP 上传到测试服务器。
- 当前测试服务器：`123.56.186.169`，测试域名：`https://test.lingyigewo.me/`。
- 当前测试镜像标签：`sub2api:ltzf-c11dc55d`。
- 当前测试数据库和 Redis 均为新服务器独立实例，与生产环境不共享。
