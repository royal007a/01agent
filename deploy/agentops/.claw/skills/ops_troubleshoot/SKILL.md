---
name: ops_troubleshoot
description: Nginx 故障排查与修复标准作业程序 (SOP)。当人类报告 "服务 502"、"接口不通"、"Nginx 起不来" 或要求排查 Nginx 错误时，必须强制加载并遵循此技能。
---
# Nginx 故障排查 SOP
1. **信息收集**：首先使用 `bash` 检查 `error.log` 的最后 50 行（例如执行：`tail -n 50 error.log`），不要整份读取大日志。
2. **根因定位**：如果发现是 "upstream prematurely closed connection" 或配置文件的语法错误（unknown directive），立即检查 `nginx.conf` 的具体内容。
3. **精准修复**：确认配置错误后，绝对不能用 bash 的 sed 盲目替换，**必须使用 `edit_file` 工具**，提供足够上下文进行精准修正。
4. **配置校验**：修复后先运行 `nginx -t` 校验配置语法。
5. **服务重启**：校验通过后运行 `nginx -s reload` 使配置生效。系统会触发审批拦截，请说明重启理由并等待放行。
