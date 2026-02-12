# TVBOX节目单接口

## 主要功能
把静态EPG节目单转换成TVBox使用的节目单接口，接口组件如下

- 定时任务：使用 Go 的cron库实现每日一次的下载任务
- 文件处理：下载 gz 文件→解压→读取 XML 内容
- 数据存储：使用内存 + 文件缓存的方式存储解析后的节目数据（兼顾性能和持久化）
- HTTP 接口：实现参数解析、数据查询、JSON 格式返回

## 部署方法
复制config.yaml.sample文件改名为config.yaml（默认填写了老张的[EPG发布地址](http://epg.51zmt.top:8000/e.xml.gz)，感谢老张），放在编译好的程序同目录下直接执行程序即可

## 接口使用方法
在TVBox中直接填写到EPG接口地址即可，格式为`http://[ip]:[port]/?ch={name}&date={date}`
