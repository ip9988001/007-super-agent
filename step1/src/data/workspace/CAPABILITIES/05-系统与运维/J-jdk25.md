# jdk25 工具链（java / javac / jar / javap / jshell / jwebserver）
## 一句话
`/data/tools/jdk25` = **Temurin JDK 25.0.5**（bin 下 30 个工具），是 Ghidra 12.1.4 的必需运行时；注意 **PATH 里的 `java` 其实是 JDK 17**，两套并存。

## 实际效果（实测 2026-10-26）
- `/data/tools/jdk25/bin/java -version` → `openjdk version "25.0.5" 2026-10-20 LTS` / `Temurin-25.0.5+8` ✅
- 裸 `java -version`（走 PATH）→ **`17.0.21`**，因为 `/usr/bin/java` → `/etc/alternatives/java` → `/usr/lib/jvm/java-17-openjdk-amd64/bin/java`
- 编译+运行：`javac -d /tmp/jout H.java` → `java -cp /tmp/jout H` 输出 `hello jdk25` ✅
- `jar --create --file /tmp/h.jar H.class` → `jar --list --file /tmp/h.jar` 输出 `META-INF/`、`META-INF/MANIFEST.MF`、`H.class` ✅
- `javap -p -c /tmp/jout/H.class` → 打出 `aload_0` / `invokespecial #1` / `getstatic #7` / `ldc #13  // String hello jdk25` 等字节码 ✅（`javap -version` → 25.0.5）
- `jwebserver -b 127.0.0.1 -p 8899 -d /tmp/jwt` → `Serving /tmp/jwt and subdirectories on 127.0.0.1 port 8899`，`curl http://127.0.0.1:8899/` 拿到 `<h1>hi</h1>`，访问日志 `"GET / HTTP/1.1" 200` ✅（`jwebserver --version` → 25.0.5）
- `echo 'System.out.println("jshell ok " + (1+1));' | jshell -q` → `jshell ok 2` ✅
- Ghidra 侧：`/data/tools/ghidra/Ghidra/application.properties` → `application.java.min=21`、`application.version=12.1.4`；跑 `support/analyzeHeadless` 实际打印 **`openjdk 25.0.5`** → launch.sh 发现 PATH 里的 17 太旧，自动回落到 `$JAVA_HOME=/data/tools/jdk25` ✅

## 怎么调
```bash
export JAVA_HOME=/data/tools/jdk25
export PATH="/data/tools/jdk25/bin:$PATH"        # 不前置就只能写全路径

/data/tools/jdk25/bin/javac -d /tmp/out Foo.java
/data/tools/jdk25/bin/jar --list --file app.jar
/data/tools/jdk25/bin/jar --extract --file app.jar --destination /tmp/unpack
/data/tools/jdk25/bin/javap -p -c /tmp/out/Foo.class      # 反编译成字节码（含私有成员）
/data/tools/jdk25/bin/jshell -q                            # 交互/管道跑片段
/data/tools/jdk25/bin/jwebserver -b 127.0.0.1 -p 8899 -d /tmp/site &   # 临时静态服务
```
与逆向栈的关系：**Ghidra（`02/G-ghidra.md`）与 REA（`02/R-rea.md`）靠它跑**（Ghidra 要 ≥21）；手上有 jar/class 时先用 `jar --list` + `javap -p -c` 把结构摊开，再决定要不要上 Ghidra/REA。

## 坑
- `start.sh` 第 10-11 行确实 `export JAVA_HOME` 并把 jdk25 前置进 PATH，但 **agent 子 shell 的 PATH 里没有**（实测只有 `/root/.pi/agent/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`）→ 裸 `java` 是 17。要 25 就写全路径。
- Ghidra 的 `support/launch.sh` "try PATH first"（第 128 行）会先校验版本再回落 `JAVA_HOME`，所以现在能跑；一旦你把 jdk17 再往前塞，它就直接失败。
- `jar --extract` 会**静默覆盖**目标目录同名文件，先解到空目录。
- `jwebserver` 是 JDK 18+ 的简易静态服务器，只适合临时验证产物；它会打印每条访问日志，长跑要重定向。
- 别把 `/data/tools/jdk25/bin` 里的 `java` 软链到 `/usr/bin`——`/usr/bin/java` 归 alternatives 管，改了就影响 JDK 17 依赖的系统工具。
