# fn-nspawn —— nspawn容器（飞牛 fnOS 原生 systemd-nspawn 可视化管理器）

面向飞牛私有云（fnOS）的轻量化原生系统容器可视化管理器（**Go 单二进制后端 + 飞牛统一网关 Unix Socket + 内嵌 Web UI & 独立网页终端**）。

软件本身只负责编排与管理，容器的根文件系统、格式化配置及镜像快照全部持久化在各自独立的容器目录内，支持**宿主机开机自启、重装系统/换盘一键恢复、外部 `systemd-nspawn` 容器自动发现与无损安全迁移**。

---

## 标准化容器目录布局

新建或迁移后的标准容器默认存放在设定的「容器标准数据目录」（默认 `/vol1/nspawn`）下：

```text
<StandardBaseRoot>/<machine>/
├── rootfs/                        # 容器根文件系统 (推荐 Btrfs 子卷)
├── config/                        # 持久化配置与运行文件
│   ├── container.json             # 格式化声明式配置（唯一真相源）
│   ├── nspawn-<machine>.service   # systemd 服务单元模板（会同步实体副本至 /etc/systemd/system/）
│   ├── network-up.sh              # 启动后将 ve-<machine> 自动接入物理网卡所在 OVS 网桥
│   ├── network-down.sh            # 停止后清理 OVS 端口
│   └── restore.sh                 # 脱离 Web 面板也可独立执行的命令行恢复脚本
└── images/                        # 镜像与快照归档
    ├── snapshots/                 # Btrfs 子卷快照 (<machine>-<tag>-<time>)
    └── exports/                   # 导出的 rootfs 镜像归档 (.tar.gz / .tar.zst)
```

---

## 核心特性

1. **深度适配飞牛 fnOS 的开机自启与「修复」机制**：
   - 针对 fnOS 存储池延迟挂载特性，将实体 `.service` 副本同步至 `/etc/systemd/system/` 并内置 `ExecStartPre` 轮询等待 `rootfs` 挂载与 OVS 网桥就绪，彻底避免软链断链导致 `systemd` 跳过自启或缓存 `not-found`。
   - 应用启动时异步巡检未同步的旧单元与开机自启兜底状态；详情页提供带确认弹窗的 **「修复」** 操作，可随时按 `container.json` 重新校验并同步单元与网络。
2. **外部容器自动发现与无损一键迁移**：
   - 自动扫描标准目录与补充扫描目录（兼容旧版 `<machine>-conf/` 结构），同时通过 `machinectl` 自动发现系统中以其他方式运行中的外部 `systemd-nspawn` 容器（提供只读安全保护，不破坏原外部配置）。
   - 支持一键将外部容器或非标准路径容器**安全迁移为标准容器**：先停止旧容器 → 同池 Btrfs 瞬时快照或 `rsync`/`cp -a` 无损复刻 `rootfs` → 生成标准 5 文件 → 清理旧启动项，**完整保留全部原始数据**由用户自行决定是否清理。
3. **网卡自动识别与 OVS 状态检查**：
   - 自动枚举宿主机物理网卡并标注是否已接入 OVS 网桥；所有创建、导入、配置、迁移与设置入口均实时提示未开启 OVS 的网卡风险。
4. **多种容器构建方式与快照/镜像体系**：
   - 支持 **Debian / Ubuntu 在线构建（`debootstrap`）** 以及 **本地 `.tar.gz` / `.tar.zst` rootfs 归档导入**；
   - 支持 **Btrfs 子卷瞬时快照、只读基线、安全回滚** 以及 **无停机一致性镜像导出**。
5. **多容器独立网页终端、实时日志与响应式适配**：
   - 基于 `xterm.js` + WebSocket PTY（`machinectl shell`），每个容器保留独立终端会话互不干扰；
   - `journalctl` 实时日志流，内置可拖动侧栏宽度调节与深色/浅色主题，适配桌面浏览器与飞牛手机 App 视图。

---

## 运行前提条件（宿主机环境）

- 飞牛 fnOS（或支持以下组件的 Debian/Linux x86_64/arm64 宿主机）：
  - `systemd` / `systemd-nspawn` / `machinectl`
  - **Open vSwitch (`ovs-vsctl`)**：物理网卡需在飞牛网络设置中启用 OVS（容器依赖 OVS 网桥获取独立局域网 IP）
  - `btrfs-progs`（强烈推荐容器目录位于 Btrfs 分区以使用子卷快照与零拷贝克隆）
  - `debootstrap`（在线构建容器时需要）、`rsync`（推荐，跨盘迁移使用）、`zstd`（可选，导出 `.tar.zst`）

---

## 构建 `.fpk` 安装包

### 准备 `fnpack` 工具

从飞牛官方开发者渠道获取对应平台的 `fnpack`（例如 `fnpack.exe`），放入 `tools/` 目录（该目录已加入 `.gitignore`，不会提交到 Git）。

### 方式一：Windows 交叉编译并打包

安装 Go（1.22+）后在 `fn-nspawn/` 目录运行 PowerShell：

```powershell
.\scripts\build.ps1 -Arch amd64
```

### 方式二：Linux / 容器内构建

```bash
bash scripts/build.sh
```

构建脚本会自动编译静态 Linux 二进制至 `fpk/app/bin/fn-nspawn`，规范化 `fpk/` 脚本换行与权限，并在 `dist/` 下生成 `fn-nspawn-v<version>-<platform>.fpk`。

---

## 安装与调试

1. 打开飞牛桌面「**应用中心 → 手动安装应用**」，上传打包生成的 `.fpk` 文件；
2. 首次安装向导可填写默认「容器标准数据目录」（默认 `/vol1/nspawn`），安装后可在界面左下角 **设置** 中随时可视化浏览更改；
3. 该应用通过飞牛统一网关与管理员权限鉴权，仅限 NAS 管理员访问。

### 宿主机独立开发调试

```bash
go build -o fn-nspawn .
sudo ./fn-nspawn --dev --listen 127.0.0.1:8099 --socket ""
```

---

## License

[MIT License](LICENSE) © 2026 [xwndo](https://github.com/xwndo)
