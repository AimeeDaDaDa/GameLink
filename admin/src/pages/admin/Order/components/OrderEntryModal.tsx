/**
 * 录入订单弹窗（参考 delta-settlement 录单工作台）。
 * P1 普通单：一名主陪玩，实时报价预览，保存草稿 / 保存并提交。
 */
import React, { useState, useEffect } from 'react';
import {
  Modal,
  Form,
  Select,
  Input,
  InputNumber,
  DatePicker,
  Row,
  Col,
  Divider,
  Space,
  Button,
  Typography,
  message,
} from 'antd';
import dayjs from 'dayjs';
import {
  adminApi,
  type OrderEntryQuote,
  type OrderEntryOptions,
  type User,
  type Game,
  type ServiceItem,
  type Player,
} from '@/api/admin';
import { logger } from '@/utils/logger';

const { Text } = Typography;

interface Props {
  visible: boolean;
  onClose: () => void;
  onSuccess: () => void;
}

const fen = (v?: number) => Math.round((v || 0) * 100);
const yuan = (cents: number) => `¥${(cents / 100).toFixed(2)}`;

const OrderEntryModal: React.FC<Props> = ({ visible, onClose, onSuccess }) => {
  const [form] = Form.useForm();
  const [submitting, setSubmitting] = useState(false);
  const [users, setUsers] = useState<User[]>([]);
  const [games, setGames] = useState<Game[]>([]);
  const [serviceItems, setServiceItems] = useState<ServiceItem[]>([]);
  const [players, setPlayers] = useState<Player[]>([]);
  const [options, setOptions] = useState<OrderEntryOptions | null>(null);
  const [quote, setQuote] = useState<OrderEntryQuote | null>(null);

  const platform = Form.useWatch('platform', form);
  const gameId = Form.useWatch('gameId', form);
  const grossAmount = Form.useWatch('grossAmount', form);
  const tip = Form.useWatch('tip', form);
  const discount = Form.useWatch('discount', form);
  const playerId = Form.useWatch('playerId', form);
  const clubRate = Form.useWatch('clubRate', form);

  // 打开时加载 options 与下拉数据
  useEffect(() => {
    if (!visible) return;
    (async () => {
      try {
        const [optsRes, usersRes, gamesRes, playersRes, itemsRes] = await Promise.all([
          adminApi.getOrderEntryOptions(),
          adminApi.getUsers({ page_size: 1000 }),
          adminApi.getGames({ page_size: 1000 }),
          adminApi.getPlayers({ page_size: 1000 }),
          adminApi.getServiceItems({ page_size: 1000 }),
        ]);
        if (optsRes.data.success) setOptions(optsRes.data.data);
        if (usersRes.data.success) setUsers(usersRes.data.data || []);
        if (gamesRes.data.success) setGames(gamesRes.data.data || []);
        if (playersRes.data.success) setPlayers(playersRes.data.data || []);
        if (itemsRes.data.success) {
          const raw = itemsRes.data.data as unknown;
          setServiceItems(
            Array.isArray(raw) ? (raw as ServiceItem[]) : ((raw as { items?: ServiceItem[] }).items || [])
          );
        }
      } catch (e) {
        logger.error('Load order entry options error:', e);
      }
    })();
  }, [visible]);

  // 切换平台时更新默认抽成
  useEffect(() => {
    if (!visible || !platform || !options) return;
    const rate = options.default_rates[platform];
    if (rate !== undefined && form.getFieldValue('clubRate') === undefined) {
      form.setFieldsValue({ clubRate: rate });
    }
  }, [visible, platform, options, form]);

  const filteredItems = React.useMemo(() => {
    if (!gameId) return serviceItems.filter((s) => !s.gameId);
    return serviceItems.filter((s) => !s.gameId || s.gameId === gameId);
  }, [serviceItems, gameId]);

  // 实时报价（300ms 防抖）
  useEffect(() => {
    if (!visible || !grossAmount || !playerId) {
      setQuote(null);
      return;
    }
    const timer = setTimeout(async () => {
      try {
        const res = await adminApi.quoteOrderEntry({
          order_type: 'normal',
          gross_amount_cents: fen(grossAmount),
          tip_cents: fen(tip),
          discount_cents: fen(discount),
          participants: [{ player_id: playerId, role: 'primary', club_rate_bps: clubRate ?? 0 }],
        });
        if (res.data.success) setQuote(res.data.data);
      } catch {
        setQuote(null);
      }
    }, 300);
    return () => clearTimeout(timer);
  }, [visible, grossAmount, tip, discount, playerId, clubRate]);

  const handleSubmit = async (action: 'draft' | 'submit') => {
    try {
      const values = await form.validateFields();
      setSubmitting(true);
      const res = await adminApi.createOrderEntry(
        {
          action,
          customer_id: values.customerId,
          business_date: values.businessDate
            ? dayjs(values.businessDate).format('YYYY-MM-DD')
            : dayjs().format('YYYY-MM-DD'),
          game_id: values.gameId,
          item_id: values.itemId,
          platform: values.platform || 'pc',
          order_type: 'normal',
          theme: values.theme,
          note: values.note,
          collection_mode: values.collectionMode || 'uncollected',
          gross_amount_cents: fen(values.grossAmount),
          tip_cents: fen(values.tip),
          discount_cents: fen(values.discount),
          participants: [{ player_id: values.playerId, role: 'primary', club_rate_bps: values.clubRate ?? 0 }],
        },
        crypto.randomUUID()
      );
      if (res.data.success) {
        message.success(action === 'submit' ? '录单已提交' : '草稿已保存');
        onSuccess();
        onClose();
      } else {
        message.error(res.data.message || '保存失败');
      }
    } catch (e) {
      logger.error('Order entry error:', e);
      message.error('保存失败');
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Modal
      title="录入订单"
      open={visible}
      onCancel={onClose}
      width={960}
      footer={
        <Space>
          <Button onClick={onClose}>取消</Button>
          <Button loading={submitting} onClick={() => handleSubmit('draft')}>
            保存草稿
          </Button>
          <Button type="primary" loading={submitting} onClick={() => handleSubmit('submit')}>
            保存并提交
          </Button>
        </Space>
      }
    >
      <Form form={form} layout="vertical" initialValues={{ platform: 'pc', orderType: 'normal', collectionMode: 'uncollected', businessDate: dayjs() }}>
        <Row gutter={16}>
          <Col span={12}>
            <Form.Item name="customerId" label="老板（客户）" rules={[{ required: true, message: '请选择老板' }]}>
              <Select
                showSearch
                placeholder="搜索并选择老板"
                optionFilterProp="label"
                options={users.map((u) => ({
                  label: `${u.name}（${u.email || u.phone || `ID:${u.id}`}）`,
                  value: u.id,
                }))}
              />
            </Form.Item>
          </Col>
          <Col span={12}>
            <Form.Item name="businessDate" label="业务日期" rules={[{ required: true, message: '请选择业务日期' }]}>
              <DatePicker style={{ width: '100%' }} />
            </Form.Item>
          </Col>
          <Col span={12}>
            <Form.Item name="platform" label="平台" rules={[{ required: true, message: '请选择平台' }]}>
              <Select
                options={(options?.platforms || ['pc', 'mobile']).map((p) => ({
                  label: p === 'pc' ? '端游' : '手游',
                  value: p,
                }))}
              />
            </Form.Item>
          </Col>
          <Col span={12}>
            <Form.Item name="gameId" label="游戏" rules={[{ required: true, message: '请选择游戏' }]}>
              <Select
                showSearch
                placeholder="选择游戏"
                optionFilterProp="label"
                options={games.map((g) => ({ label: g.name, value: g.id }))}
              />
            </Form.Item>
          </Col>
          <Col span={12}>
            <Form.Item name="itemId" label="服务项目" rules={[{ required: true, message: '请选择服务项目' }]}>
              <Select
                placeholder={gameId ? '选择服务项目' : '请先选择游戏'}
                options={filteredItems.map((s) => ({
                  label: `${s.name}（${yuan(s.basePriceCents)}）`,
                  value: s.id,
                }))}
              />
            </Form.Item>
          </Col>
          <Col span={12}>
            <Form.Item name="playerId" label="主陪玩" rules={[{ required: true, message: '请选择主陪玩' }]}>
              <Select
                showSearch
                placeholder="选择陪玩"
                optionFilterProp="label"
                options={players.map((p) => ({ label: `${p.nickname}（ID:${p.id}）`, value: p.id }))}
              />
            </Form.Item>
          </Col>
          <Col span={12}>
            <Form.Item name="clubRate" label="俱乐部抽成（基点，1500=15%）" rules={[{ required: true, message: '请填写抽成' }]}>
              <InputNumber min={0} max={10000} step={50} style={{ width: '100%' }} />
            </Form.Item>
          </Col>
          <Col span={12}>
            <Form.Item name="grossAmount" label="原订单金额（元）" rules={[{ required: true, message: '请填写原价' }]}>
              <InputNumber min={0.01} precision={2} prefix="¥" style={{ width: '100%' }} placeholder="原价" />
            </Form.Item>
          </Col>
          <Col span={6}>
            <Form.Item name="tip" label="老板打赏（元）" initialValue={0}>
              <InputNumber min={0} precision={2} prefix="¥" style={{ width: '100%' }} />
            </Form.Item>
          </Col>
          <Col span={6}>
            <Form.Item name="discount" label="优惠金额（元）" initialValue={0}>
              <InputNumber min={0} precision={2} prefix="¥" style={{ width: '100%' }} />
            </Form.Item>
          </Col>
          <Col span={12}>
            <Form.Item name="collectionMode" label="收款方式">
              <Select
                options={(options?.collection_modes || ['uncollected', 'manual']).map((m) => ({
                  label: m === 'uncollected' ? '暂不收款' : m === 'manual' ? '人工收款' : m,
                  value: m,
                }))}
              />
            </Form.Item>
          </Col>
          <Col span={12}>
            <Form.Item name="theme" label="主题玩法（可选）">
              <Input placeholder="如：三角洲护航" maxLength={100} />
            </Form.Item>
          </Col>
          <Col span={12}>
            <Form.Item name="note" label="备注（可选）">
              <Input.TextArea rows={2} placeholder="备注" maxLength={500} />
            </Form.Item>
          </Col>
        </Row>

        {quote && (
          <>
            <Divider style={{ margin: '8px 0' }} />
            <div
              style={{
                background: '#f7f9fc',
                border: '1px solid #e8ecf1',
                borderRadius: 8,
                padding: '12px 16px',
              }}
            >
              <Space size={24} wrap>
                <Text>
                  老板应付：<Text strong style={{ color: '#f5222d' }}>{yuan(quote.payable_cents)}</Text>
                </Text>
                <Text>
                  俱乐部抽成：<Text strong style={{ color: '#818cf8' }}>{yuan(quote.club_commission_cents)}</Text>
                </Text>
                <Text>
                  优惠承担：<Text strong>{yuan(quote.discount_cents)}</Text>
                </Text>
                <Text>
                  俱乐部净额：<Text strong>{yuan(quote.club_net_cents)}</Text>
                </Text>
                <Text>
                  陪玩应结算：<Text strong style={{ color: '#2d9d46' }}>
                    {quote.allocations.length > 0 ? yuan(quote.allocations[0].settlement_cents) : '-'}
                  </Text>
                </Text>
              </Space>
              <div style={{ marginTop: 4, color: '#8c8c8c', fontSize: 12 }}>
                金额为预估，押金按正式入账时余额确认；规则版本 {quote.rule_version}
              </div>
            </div>
          </>
        )}
      </Form>
    </Modal>
  );
};

export default OrderEntryModal;
