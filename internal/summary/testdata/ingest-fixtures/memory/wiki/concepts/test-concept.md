# 定时器与事件循环

Node 里 setInterval 会让事件循环一直有活干，进程收不到退出信号。
想让进程正常退出，定时器要加 unref，或者在退出前 clearInterval。
