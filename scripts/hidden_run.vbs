' ===========================================================================
'  hidden_run.vbs —— 用「完全隐藏」的窗口启动一个程序
'  ---------------------------------------------------------------------------
'  为什么需要它：
'    cmd 里 `start /min exe` 只是把控制台窗口最小化，任务栏仍然挂着图标；
'    真正的「无窗口」在 Windows 上要么走服务（Session 0），要么用
'    WScript.Shell.Run 的第 2 个参数 0（SW_HIDE）。
'    本机 okxweb 平时是 Windows 服务（scripts\start_all.bat 优先走服务），
'    只有在服务注册不上（非管理员）时才退回这里，保证依然看不到黑窗口。
'
'  用法：
'    wscript.exe /B /Nologo hidden_run.vbs "C:\path\app.exe" "arg1 arg2"
'    （第 2 个及之后的参数会原样拼在 exe 后面；含空格请整段加引号）
' ===========================================================================
Option Explicit

Dim sh, exePath, extraArgs, i

If WScript.Arguments.Count = 0 Then
    WScript.Quit 1
End If

exePath = WScript.Arguments(0)

extraArgs = ""
For i = 1 To WScript.Arguments.Count - 1
    extraArgs = extraArgs & " " & WScript.Arguments(i)
Next

Set sh = CreateObject("WScript.Shell")
' 0     = 窗口隐藏（SW_HIDE）
' False = 不等待子进程退出（脚本立刻结束，进程继续常驻）
sh.Run """" & exePath & """" & extraArgs, 0, False
