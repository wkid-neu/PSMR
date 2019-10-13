## 版本5

- 基于并发hashbitmap的graph版本
- 截止2019/9/23，数据就是在这个版本上测试的
- 由于系统处理能力太快，不能显示调度器的性能，我们模拟一次apply的速度大约为0.2ms。
- 吞吐量计算方式：由于go rpc调用和实现的paxos协议的速度问题，我们测量系统正在处理的中间状态的系统吞吐量，即从第500000条Trans应用开始，到第150000条应用结束。
- 此系统不符合线性一致性模型，但调度器的测试数据准确。
