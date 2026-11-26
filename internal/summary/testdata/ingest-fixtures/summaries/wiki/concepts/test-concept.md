summary: Node 定时器不加 unref 会钉住事件循环，进程收不到退出信号一直跑，退出前要先 clearInterval 或者给定时器加 unref。
triggers: 进程关不掉，定时器转不停，Node 退不出来，unref 用法，服务停不下来
