package track

import (

	// . "github.com/logrusorgru/aurora"

	"go.uber.org/zap"
	"m7s.live/engine/v4/codec"
	"m7s.live/engine/v4/common"
	. "m7s.live/engine/v4/common"
	"m7s.live/engine/v4/util"
)

type Video struct {
	Media
	CodecID     codec.VideoCodecID
	GOP         int  //关键帧间隔
	nalulenSize int  //avcc格式中表示nalu长度的字节数，通常为4
	dcChanged   bool //解码器配置是否改变了，一般由于变码率导致
	dtsEst      *DTSEstimator
	lostFlag    bool // 是否丢帧
	codec.SPSInfo
	ParamaterSets `json:"-"`
	SPS           []byte `json:"-"`
	PPS           []byte `json:"-"`
}

func (v *Video) Attach() {
	if v.Attached.CompareAndSwap(false, true) {
		promise := util.NewPromise(common.Track(v))
		v.Stream.AddTrack(promise)
		if err := promise.Await(); err != nil {
			v.Error("attach video track failed", zap.Error(err))
		} else {
			v.Info("video track attached", zap.Uint("width", v.Width), zap.Uint("height", v.Height))
		}
	}
}

func (v *Video) Detach() {
	if v.Attached.CompareAndSwap(true, false) {
		v.Stream.RemoveTrack(v)
	}
}

func (vt *Video) GetName() string {
	if vt.Name == "" {
		return vt.CodecID.String()
	}
	return vt.Name
}

// PlayFullAnnexB 订阅annex-b格式的流数据，每一个I帧增加sps、pps头
// func (vt *Video) PlayFullAnnexB(ctx context.Context, onMedia func(net.Buffers) error) error {
// 	for vr := vt.ReadRing(); ctx.Err() == nil; vr.MoveNext() {
// 		vp := vr.Read(ctx)
// 		var data net.Buffers
// 		if vp.IFrame {
// 			data = vt.GetAnnexB()
// 		}
// 		data = append(data, codec.NALU_Delimiter2)
// 		for slice := vp.AUList.Head; slice != nil; slice = slice.Next {
// 			data = append(data, slice.ToBuffers()...)
// 			if slice.Next != nil {
// 				data = append(data, codec.NALU_Delimiter1)
// 			}
// 		}

//			if err := onMedia(data); err != nil {
//				// TODO: log err
//				return err
//			}
//		}
//		return ctx.Err()
//	}
func (vt *Video) computeGOP() {
	if vt.IDRing != nil {
		vt.GOP = int(vt.Value.Sequence - vt.IDRing.Value.Sequence)
		if vt.HistoryRing == nil {
			vt.narrow(vt.GOP)
		}
	}
	vt.AddIDR()
	// var n int
	// for i := 0; i < len(vt.BytesPool); i++ {
	// 	n += vt.BytesPool[i].Length
	// }
	// println(n)
}

func (vt *Video) writeAnnexBSlice(nalu []byte) {
	common.SplitAnnexB(nalu, vt.WriteSliceBytes, codec.NALU_Delimiter1)
}

func (vt *Video) WriteAnnexB(pts uint32, dts uint32, frame []byte) {
	if dts == 0 {
		vt.generateTimestamp(pts)
	} else {
		vt.Value.PTS = pts
		vt.Value.DTS = dts
	}
	vt.Value.BytesIn += len(frame)
	common.SplitAnnexB(frame, vt.writeAnnexBSlice, codec.NALU_Delimiter2)
	vt.Flush()
}

func (vt *Video) WriteAVCC(ts uint32, frame *util.BLL) error {
	r := frame.NewReader()
	b, err := r.ReadByte()
	if err != nil {
		return err
	}
	b = b >> 4
	vt.Value.IFrame = b == 1 || b == 4
	r.ReadByte() //sequence frame flag
	cts, err := r.ReadBE(3)
	if err != nil {
		return err
	}
	vt.Value.PTS = vt.Ms2MpegTs(ts + cts)
	vt.Value.DTS = vt.Ms2MpegTs(ts)
	// println(":", vt.Value.Sequence)
	for nalulen, err := r.ReadBE(vt.nalulenSize); err == nil; nalulen, err = r.ReadBE(vt.nalulenSize) {
		// var au util.BLL
		// for _, bb := range r.ReadN(int(nalulen)) {
		// 	au.Push(vt.BytesPool.GetShell(bb))
		// }
		// println(":", nalulen, au.ByteLength)
		// vt.Value.AUList.PushValue(&au)
		vt.AppendAuBytes(r.ReadN(int(nalulen))...)
	}
	vt.Value.WriteAVCC(ts, frame)
	// {
	// 	b := util.Buffer(vt.Value.AVCC.ToBytes()[5:])
	// 	println(vt.Value.Sequence)
	// 	for b.CanRead() {
	// 		nalulen := int(b.ReadUint32())
	// 		if b.CanReadN(nalulen) {
	// 			bb := b.ReadN(int(nalulen))
	// 			println(nalulen, codec.ParseH264NALUType(bb[0]))
	// 		} else {
	// 			println("error")
	// 		}
	// 	}
	// }
	vt.Flush()
	return nil
}

func (vt *Video) WriteSliceByte(b ...byte) {
	vt.WriteSliceBytes(b)
}

// 在I帧前面插入sps pps webrtc需要
func (vt *Video) insertDCRtp() {
	head := vt.Value.RTP.Next
	seq := head.Value.SequenceNumber
	for _, nalu := range vt.ParamaterSets {
		var packet RTPFrame
		packet.Version = 2
		packet.PayloadType = vt.PayloadType
		packet.Payload = nalu
		packet.SSRC = vt.SSRC
		packet.Timestamp = vt.Value.PTS
		packet.Marker = false
		head.InsertBeforeValue(packet)
		vt.rtpSequence++
	}
	vt.Value.RTP.RangeItem(func(item *util.ListItem[RTPFrame]) bool {
		item.Value.SequenceNumber = seq
		seq++
		return true
	})
}

func (vt *Video) generateTimestamp(ts uint32) {
	vt.Value.PTS = ts
	vt.Value.DTS = vt.dtsEst.Feed(ts)
}

func (vt *Video) SetLostFlag() {
	vt.lostFlag = true
}
func (vt *Video) CompleteAVCC(rv *AVFrame) {
	mem := vt.BytesPool.Get(5)
	b := mem.Value
	if rv.IFrame {
		b[0] = 0x10 | byte(vt.CodecID)
	} else {
		b[0] = 0x20 | byte(vt.CodecID)
	}
	b[1] = 1
	// println(rv.PTS < rv.DTS, "\t", rv.PTS, "\t", rv.DTS, "\t", rv.PTS-rv.DTS)
	// 写入CTS
	util.PutBE(b[2:5], vt.MpegTs2Ms(rv.PTS-rv.DTS))
	rv.AVCC.Push(mem)
	// if rv.AVCC.ByteLength != 5 {
	// 	panic("error")
	// }
	// var tmp = 0
	rv.AUList.Range(func(au *util.BLL) bool {
		mem = vt.BytesPool.Get(4)
		// println(au.ByteLength)
		util.PutBE(mem.Value, uint32(au.ByteLength))
		rv.AVCC.Push(mem)
		au.Range(func(slice util.Buffer) bool {
			rv.AVCC.Push(vt.BytesPool.GetShell(slice))
			return true
		})
		// tmp += 4 + au.ByteLength
		// if rv.AVCC.ByteLength != 5+tmp {
		// 	panic("error")
		// }
		return true
	})
}

func (vt *Video) Flush() {
	rv := &vt.Value
	if rv.IFrame {
		vt.computeGOP()
		vt.Stream.SetIDR(vt)
	}
	if !vt.Attached.Load() && vt.IDRing != nil && vt.SequenceHeadSeq > 0 {
		defer vt.Attach()
	}

	if vt.lostFlag {
		if rv.IFrame {
			vt.lostFlag = false
		} else {
			rv.Reset()
			return
		}
	}
	vt.Media.Flush()
	vt.dcChanged = false
}

func (vt *Video) WriteSequenceHead(sh []byte) {
	vt.Media.WriteSequenceHead(sh)
	vt.dcChanged = true
}

/*
Access Unit的首个nalu是4字节起始码。
这里举个例子说明，用JM可以生成这样一段码流（不要使用JM8.6，它在这部分与标准不符），这个码流可以见本楼附件：
    SPS          （4字节头）
    PPS          （4字节头）
    SEI          （4字节头）
    I0(slice0)     （4字节头）
    I0(slice1)   （3字节头）
    P1(slice0)     （4字节头）
    P1(slice1)   （3字节头）
    P2(slice0)     （4字节头）
    P2(slice1)   （3字节头）
I0(slice0)是序列第一帧（I帧）的第一个slice，是当前Access Unit的首个nalu，所以是4字节头。而I0(slice1)表示第一帧的第二个slice，所以是3字节头。P1(slice0) 、P1(slice1)同理。

*/
