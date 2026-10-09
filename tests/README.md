# 测试目录与运行方式

测试按所属包归档，不建立混合所有模块的集中目录：

```text
agent/tests/*_test.go
cmd/artex/tests/*_test.go
db/tests/*_test.go
server/tests/*_test.go
norma/llm/tests/*_test.go
web/src/lib/tests/*.test.mjs
tests/                      # 仓库级测试入口及发布脚本测试
```

## Go 测试

Go 将子目录视为独立包，而现有白盒测试依赖所属包的未导出成员。因此测试源码用 `//go:build ignore` 避免被当作子包编译；`run.go` 在临时目录去掉这一头部，生成 Go 原生 `-overlay` 映射，把测试**虚拟**映射回原包。

不移动业务代码、不导出内部 API、不往业务目录复制文件。包身份、TestMain、测试夹具相对路径和 coverage 都保持原包语义。临时源码和 overlay 在运行结束后清理。**直接执行 `go test ./...` 不会加载迁移后的测试，必须使用以下入口。**

在仓库根目录运行：

```bash
go run ./tests/run.go                          # ARTEX 主模块
go run ./tests/run.go --module norma           # 独立 norma 模块
go run ./tests/run.go --all                    # 两个模块；某模块失败仍继续检查另一个
go run ./tests/run.go ./config ./notify -count=1
go run ./tests/run.go -race ./db ./server -run TestSide -count=1
go run ./tests/run.go --all -run '^$'           # 编译全部测试，不执行测试用例
go test ./tests/run.go ./tests/run_test.go      # 入口自身的单元 / 集成测试
```

`--all` / `--module` 位于入口参数最前面，剩余参数转给 `go test`；未指定包时默认 `./...`。跨两个模块执行时，包参数必须在两个模块中都存在（通常使用默认 `./...`）。Go 的 `-overlay` 参数由入口管理，不能另外覆盖。

新增 Go 测试放入所属包的 `tests/<文件名>_test.go`，仍声明原来的包名，文件开头固定为：

```go
//go:build ignore

package yourpackage
```

测试夹具保留在原包的 `testdata`；工作目录仍为原包目录。入口隔离嵌套模块、忽略 node_modules/vendor 和构建目录；不会扫描前端测试或技能参考文档。

依赖 PostgreSQL 的测试需要设置指向**可丢弃测试数据库**的 `ARTEX_PG_DSN`。不要使用生产库。未配置数据库时，部分既有集成用例会跳过，部分会失败；Windows 环境和 norma 也有既有失败用例，本次迁移没有改断言来掩盖这些问题。

## 前端与发布脚本

```bash
cd web
npm test                         # Node.js 22.6+，不需要额外测试框架
cd ..
python tests/build_test.py        # Bash + Python 3.9+；Windows 自动使用 Git Bash
```

发布脚本测试在独立临时目录执行 `build.sh`，模拟 Go 编译，但实际创建 zip、校验 SHA256 和启动脚本权限。验证全部五个平台、排除旧 zip、不支持 / 重复目标的提前失败以及校验工具失败不能被忽略；不代表已完成真实前端构建或实际发布。

CI 在分支 push、PR 和 Release 前执行入口测试、发布脚本测试、全部 Go 测试的编译检查、config/notify/selfupdate 核心包回归和前端单元测试。编译检查不是完整回归通过的声明；完整回归仍使用上面的主模块 / norma / `--all` 命令。
