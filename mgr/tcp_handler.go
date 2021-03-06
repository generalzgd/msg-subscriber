/**
 * @version: 1.0.0
 * @author: generalzgd
 * @license: LGPL v3
 * @contact: general_zgd@163.com
 * @site: github.com/generalzgd
 * @software: GoLand
 * @file: posthandler.go
 * @time: 2019/12/23 4:19 下午
 * @project: packagesubscribesvr
 */

package mgr

import (
	`encoding/json`
	`fmt`
	`time`

	`github.com/astaxie/beego/logs`
	zgdSlice `github.com/generalzgd/comm-libs/slice`
	gotcp `github.com/generalzgd/securegotcp`

	`github.com/generalzgd/msg-subscriber/codec`
	`github.com/generalzgd/msg-subscriber/define`
	`github.com/generalzgd/msg-subscriber/iface`
	`github.com/generalzgd/msg-subscriber/receive`
	`github.com/generalzgd/msg-subscriber/sender`
	`github.com/generalzgd/msg-subscriber/util`
)

func (p *Manager) StartTcpWork() error {
	limitCfg := &gotcp.Config{
		PacketSendChanLimit:    uint32(p.cfg.PostCfg.GetSendChanLimit()),
		PacketReceiveChanLimit: uint32(p.cfg.PostCfg.GetReceiveChanLimit()),
	}

	recv := receive.NewTcpReceiver(p)
	recv.SetExcludeIp(p.cfg.ExcludeIp...)
	recv.Start(limitCfg, p.cfg.PostCfg.GetListenAddr(), &receive.TcpProtocol{
		HeadSize:  p.cfg.Decode.HeadSize,
		LenPos:    p.cfg.Decode.LenPos,
		LenSize:   p.cfg.Decode.LenSize,
		HeadCodec: headDecoder,
		BodyCodec: bodyDecoder,
	})
	p.TcpReceiver = recv
	return nil
}

func (p *Manager) GetKey() string {
	return "msg subscriber manager"
}

func (p *Manager) OnConnect(iface.IPackSender) {
}

func (p *Manager) OnClose(conn iface.IPackSender) {
	connId := conn.GetConnId()
	logs.Debug("OnClose(%d, %v)", connId, conn.GetAddress())

	v, ok := p.postId2Subscriber.Load(connId)
	if !ok {
		return
	}
	info, ok := v.(iface.IConsumer)
	if !ok {
		return
	}
	// 延时清理，确保消费者偶然断开时，消息不丢
	t := time.AfterFunc(p.cfg.CleanDelay, func() {
		logs.Debug("OnClose() start clean subscribe. info=%v", info)

		ids := p.subscribeMap.GetIdsByConsumer(info)
		p.doneUnsubscribe(ids, info)
		//
		p.subscriberTimer.Delete(info.GetKey())
	})
	p.subscriberTimer.Store(info.GetKey(), t)
}

func (p *Manager) OnMessage(conn iface.IPackSender, pack gotcp.Packet) {
	dataPack := codec.NewDataPack(pack.Serialize(), headDecoder, bodyDecoder)
	//
	cmd := dataPack.GetCmdId()
	if cmd < 1 {
		logs.Error("OnMessage() got err=[empty cmdid]")
		return
	}

	handlers := map[int]func(iface.IPackSender, *codec.DataPack){
		7681: p.onSubscribeHandler,
		7683: p.onUnsubscribeHandler,
	}

	f, ok := handlers[cmd]
	if ok {
		f(conn, dataPack)
	} else {
		p.onTcpMsgHandler(conn, dataPack)
	}
}

func (p *Manager) onSubscribeHandler(conn iface.IPackSender, pack *codec.DataPack) {
	logs.Debug("onSubscribeHandler() got=%s", pack.String())
	var err error
	defer func() {
		resp := define.SubscribeAck{}
		if err != nil {
			resp.Code = 1
			resp.Msg = err.Error()
		}
		bts, _ := json.Marshal(resp)
		pack.SetHead(7682, len(bts))
		pack.SetPackBody(bts)

		if conn != nil {
			conn.Send(pack)
		}
	}()
	//
	args := define.SubscribeReq{}
	if err = json.Unmarshal(pack.GetPackBody(), &args); err != nil {
		logs.Error("parse json fail. %v", err)
		return
	}
	//
	if len(args.CmdId) == 0 || len(args.ConsumerKey) == 0 || !zgdSlice.IsEveryUniqueUint16(args.CmdId) {
		err = fmt.Errorf("args error")
		return
	}
	connId := uint32(0)
	address := ""
	if conn != nil {
		connId = conn.GetConnId()
		address = conn.GetAddress()
	}
	// remoteAddr := sender.GetAddress()
	info := &define.SubscribeInfo{
		FromType:    define.SubscribeTypeTcp,
		ConsumerKey: args.ConsumerKey,
		Sender:      sender.NewTcpSender(args.SvrName, connId, address, conn),
	}
	//
	p.doneSubscribe(util.Uint16ToInt(args.CmdId...), info)
	//
	/*time.AfterFunc(time.Second*10, func() {
		bts := []byte(`{"cmdid":"chatmessage"}`)
		pk := gocmd.PostPacket{
			Length:   uint32(len(bts)),
			CmdId:    gocmd.ID_Chatmessage,
			Body:     bts,
		}
		info.Send(&define.FlowPack{
			Index:      1,
			Data:       pk.Serialize(),
			Pack:       pk,
		})
	})*/
}

// 统一处理订阅的数据结构
func (p *Manager) doneSubscribe(cmdIds []int, consumer iface.IConsumer) {
	logs.Debug("doneSubscribe() args=[%v, %v]", cmdIds, consumer)
	// 过滤重复订阅
	cmdIds = p.subscribeMap.Put(cmdIds, consumer)
	if len(cmdIds) > 0 {
		if consumer.GetType() == define.SubscribeTypeTcp {
			p.postId2Subscriber.Store(consumer.GetConnId(), consumer)
		}
		//
		p.CleanSubscriberTimer(consumer.GetKey())
		// 同步订阅偏移量
		p.doReportSubscribeInfoOffset(true, cmdIds, consumer.GetKey())
	}
}

// 统一处理订阅的数据结构
func (p *Manager) doneUnsubscribe(cmdIds []int, consumer iface.IConsumer) {
	cmdIds = p.subscribeMap.Del(cmdIds, consumer)
	if len(cmdIds) > 0 {
		// raft同步的时候会处理
		// p.allSubscribeMap.Del(cmdIds...)
		//
		if consumer.GetType() == define.SubscribeTypeTcp {
			p.postId2Subscriber.Delete(consumer.GetConnId())
		}
		// 同步订阅偏移量
		p.doReportSubscribeInfoOffset(false, cmdIds, consumer.GetKey())
	}
}

func (p *Manager) onUnsubscribeHandler(conn iface.IPackSender, pack *codec.DataPack) {
	logs.Debug("onUnsubscribeHandler() got=%s", pack.String())
	var err error
	defer func() {
		resp := define.UnsubscribeAck{}
		if err != nil {
			resp.Code = 1
			resp.Msg = err.Error()
		}
		bts, _ := json.Marshal(resp)
		pack.SetHead(7682, len(bts))
		pack.SetPackBody(bts)
		if conn != nil {
			conn.Send(pack)
		}
	}()
	//
	args := define.UnsubscribeReq{}
	if err = json.Unmarshal(pack.GetPackBody(), &args); err != nil {
		logs.Error("parse json fail. %v", err)
		return
	}
	//
	if len(args.CmdId) == 0 || len(args.ConsumerKey) == 0 || !zgdSlice.IsEveryUniqueUint16(args.CmdId) {
		err = fmt.Errorf("args error")
		return
	}
	connId := uint32(0)
	address := ""
	if conn != nil {
		connId = conn.GetConnId()
		address = conn.GetAddress()
	}

	info := &define.SubscribeInfo{
		FromType:    define.SubscribeTypeTcp,
		ConsumerKey: args.ConsumerKey,
		Sender:      sender.NewTcpSender(args.SvrName, connId, address, conn),
	}
	p.doneUnsubscribe(util.Uint16ToInt(args.CmdId...), info)
}

// 收到消息, 存入上报队列中
func (p *Manager) onTcpMsgHandler(conn iface.IPackSender, pack *codec.DataPack) {
	/*exclude := []int{gocmd.ID_SubscribeReq, gocmd.ID_UnsubscribeReq, gocmd.ID_SubscribeAck, gocmd.ID_UnsubscribeAck, gocmd.ID_Keeplive}
	if slice.ContainsInt(exclude, pack.GetCmdId()) {
		return
	}*/
	logs.Debug("onPostMsgHandler() got= %s", pack.String())
	// 未订阅的消息一律过滤掉
	if p.allSubscribeMap.Has(pack.GetCmdId()) {
		index, err := p.askIncreasedIndex()
		if err != nil {
			logs.Error("doReportMsg() get increased index err. %v", err)
			return
		}

		msg := define.NewFlowPack(headDecoder, bodyDecoder)
		msg.Index = index
		msg.Pack = pack

		p.doReportMsg(msg)
	} else {
		logs.Debug("onPostMsgHandler() no subscribed. ", pack.String())
	}
}
