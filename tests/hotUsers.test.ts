import test from 'node:test';
import assert from 'node:assert';
import { withUsersApiService, planRosterUpdate, USERS_API_SOCKET_PATH } from '../src/utils/hotUsers.js';

process.env.NODE_ENV = 'test';

/**
 * Решение «можно ли применить горячо» — единственное опасное место всей фичи.
 *
 * Ошибиться здесь в одну сторону дёшево: сочли изменение сложным, сделали reload, порвали сессии —
 * то есть ровно сегодняшнее поведение. Ошибиться в другую — значит применить через ручку конфиг, в
 * котором поменялось не только «кто», но и «как»: порты, сертификаты, маршрутизация. Живой процесс
 * останется со старыми, а на диске будет лежать новый, и расхождение всплывёт при следующем
 * перезапуске ноды, когда искать будут совсем не там.
 *
 * Поэтому проверки ниже устроены так: всё, что не является ЧИСТО сменой набора абонентов, обязано
 * отвечать `null`.
 */

const BASE = {
  log: { level: 'warn' },
  inbounds: [
    { type: 'tuic', tag: 'lightning-tuic', listen: '::', listen_port: 8443, users: [{ name: 'a', uuid: 'u-a', password: 'p-a' }] },
    { type: 'hysteria2', tag: 'lightning-hy2', listen: '::', listen_port: 8444, users: [{ name: 'a', password: 'p-a' }] },
  ],
  outbounds: [{ type: 'direct', tag: 'direct' }],
};

/** Глубокая копия без structuredClone-зависимостей: конфиг — обычный JSON. */
function clone<T>(value: T): T {
  return JSON.parse(JSON.stringify(value)) as T;
}

test('служба users-api добавляется и не дублируется', () => {
  const once = withUsersApiService(BASE) as { services: { type: string; path: string }[] };
  assert.strictEqual(once.services.length, 1);
  assert.strictEqual(once.services[0].type, 'users-api');
  assert.strictEqual(once.services[0].path, USERS_API_SOCKET_PATH);

  /**
   * Идемпотентность здесь не про красоту. Подмешивание идёт на КАЖДОМ пуше, и второй экземпляр
   * службы означал бы два слушателя на одном пути — то есть ядро, которое не поднимется.
   */
  const twice = withUsersApiService(once) as { services: unknown[] };
  assert.strictEqual(twice.services.length, 1, 'служба продублирована на повторном пуше');
});

test('чужие службы сохраняются', () => {
  const withOther = { ...BASE, services: [{ type: 'derp', tag: 'derp-in' }] };
  const result = withUsersApiService(withOther) as { services: { type: string }[] };

  assert.deepStrictEqual(
    result.services.map((s) => s.type),
    ['derp', 'users-api'],
    'подмешивание затёрло службы, пришедшие из профиля панели'
  );
});

test('смена только набора абонентов — горячий путь', () => {
  const next = clone(BASE);
  next.inbounds[0].users = [
    { name: 'a', uuid: 'u-a', password: 'p-a' },
    { name: 'b', uuid: 'u-b', password: 'p-b' },
  ];

  const plan = planRosterUpdate(next, BASE);

  assert.ok(plan, 'добавление абонента не распознано как смена набора');
  assert.strictEqual(plan.length, 1, 'тронут инбаунд, который не менялся');
  assert.strictEqual(plan[0].tag, 'lightning-tuic');
  assert.strictEqual(plan[0].users.length, 2);
});

test('удаление абонента — тоже горячий путь', () => {
  /**
   * Удаление != отзыв. Абонента удаляют и просто потому, что так решил владелец; рвать из-за этого
   * сессии всем остальным незачем. Немедленность нужна там, где доступ ОТЗЫВАЮТ, и туда ведёт
   * отдельный признак `force_reload`, а не догадка по форме дифа.
   */
  const next = clone(BASE);
  next.inbounds[0].users = [];

  const plan = planRosterUpdate(next, BASE);

  assert.ok(plan);
  assert.strictEqual(plan.length, 1);
  assert.deepStrictEqual(plan[0].users, []);
});

test('меняются оба инбаунда — оба и уезжают', () => {
  const next = clone(BASE);
  next.inbounds[0].users = [];
  next.inbounds[1].users = [];

  const plan = planRosterUpdate(next, BASE);

  assert.ok(plan);
  assert.deepStrictEqual(
    plan.map((u) => u.tag),
    ['lightning-tuic', 'lightning-hy2']
  );
});

test('совпадающие конфиги дают пустой план, а не отказ', () => {
  const plan = planRosterUpdate(clone(BASE), BASE);
  assert.deepStrictEqual(plan, [], 'одинаковые конфиги сочтены за сложное изменение');
});

test('изменился порт — только reload', () => {
  const next = clone(BASE);
  next.inbounds[0].listen_port = 9443;

  assert.strictEqual(
    planRosterUpdate(next, BASE),
    null,
    'смена порта уехала бы горячим путём: живое ядро осталось бы на старом, на диске лежал бы новый'
  );
});

test('изменилось что-то вне inbounds — только reload', () => {
  const next = clone(BASE);
  next.outbounds = [{ type: 'direct', tag: 'direct' }, { type: 'block', tag: 'block' }];

  assert.strictEqual(planRosterUpdate(next, BASE), null, 'правка маршрутизации сочтена за смену набора');
});

test('добавился инбаунд — только reload', () => {
  const next = clone(BASE);
  next.inbounds.push({ type: 'tuic', tag: 'extra', listen: '::', listen_port: 8445, users: [] });

  assert.strictEqual(planRosterUpdate(next, BASE), null, 'новый инбаунд невозможно поднять через ручку');
});

test('инбаунд неподдерживаемого протокола — только reload', () => {
  /**
   * Ручка знает vless, tuic и hysteria2; на остальных она ответит 404. Узнать об этом ПОСЛЕ того,
   * как часть наборов уже применена, значит остаться в полуприменённом состоянии — поэтому список
   * протоколов проверяется до первой мутации, а не по ответу сокета.
   */
  const withShadowsocks = {
    ...BASE,
    inbounds: [{ type: 'shadowsocks', tag: 'ss-in', listen: '::', listen_port: 8500, users: [{ name: 'a' }] }],
  };
  const next = clone(withShadowsocks);
  next.inbounds[0].users = [{ name: 'b' }];

  assert.strictEqual(planRosterUpdate(next, withShadowsocks), null);
});

test('инбаунд без тега — только reload', () => {
  /** Тег и есть адрес в ручке: `PUT /inbounds/<тег>/users`. Без него обращаться некуда. */
  const untagged = {
    ...BASE,
    inbounds: [{ type: 'tuic', listen: '::', listen_port: 8443, users: [{ name: 'a' }] }],
  };
  const next = clone(untagged);
  next.inbounds[0].users = [{ name: 'b' }];

  assert.strictEqual(planRosterUpdate(next, untagged), null);
});

test('конфиг без inbounds — только reload', () => {
  assert.strictEqual(planRosterUpdate({ log: { level: 'warn' } }, BASE), null);
  assert.strictEqual(planRosterUpdate(BASE, { log: { level: 'warn' } }), null);
});

test('появилась служба users-api — только reload', () => {
  /**
   * Переходный случай первой раскатки: на диске лежит конфиг, записанный агентом без подмешивания.
   * Служба поднимается только при старте инстанса, поэтому применить её через сокет невозможно —
   * да и сокета ещё нет. Один reload на ноду, дальше горячий путь.
   */
  const next = withUsersApiService(clone(BASE));

  assert.strictEqual(planRosterUpdate(next, BASE), null, 'появление самой ручки попробовали применить через неё же');
});
