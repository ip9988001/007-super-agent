# cap-ops-jdk25

## 一句话
`/data/tools/jdk25` = Temurin JDK 25.0.5（bin 下 30 个工具），是 Ghidra 12.1.4 的必需运行时；注意 PATH 里的 `java` 其实是 JDK 17，两套并存。

## 怎么调
export JAVA_HOME=/data/tools/jdk25 export PATH="/data/tools/jdk25/bin:$PATH" # 不前置就只能写全路径 /data/tools/jdk25/bin/javac -d /tmp/out Foo.java /data/tools/jdk25/bin/jar --list --file app.jar /data/tools/jdk25/bin/jar --extract --file app.jar --destination /tmp/unpack /data/tools/jdk25/bin/javap -p -c /tmp/out/Foo.class # 反编译成字节码（含私有成员） /data/tools/jdk25/bin/jshell -q # 交互/管道跑片段 /data/tools/jdk25/bin/…

## 坑
`start.sh` 第 10-11 行确实 `export JAVA_HOME` 并把 jdk25 前置进 PATH，但 agent 子 shell 的 PATH 里没有（实测只有 `/root/.pi/agent/…；Ghidra 的 `support/launch.sh` "try PATH first"（第 128 行）会先校验版本再回落 `JAVA_HOME`，所以现在能跑；一旦你把 jdk17 再往前塞，它就直接失败。

## 实际效果
`/data/tools/jdk25/bin/java -version` → `openjdk version "25.0.5" 2026-10-20 LTS` / `Temurin-25.0.5+8` ✅；裸 `java -version`（走 PATH）→ `17.0.21`，因为 `/usr/bin/java` → `/etc/alternatives/java` → `/usr/lib/jvm/java-17-op…

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`/data/tools/jdk25/bin/java -version` → `openjdk version "25.0.5" 2026-10-20 LTS` / `Temu…；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/05-系统与运维/J-jdk25.md · 核实：2026-10-26（原文件 实际效果（实测 2026-10-26））
> 注意：Temurin JDK 25.0.5；Ghidra 必需运行时
