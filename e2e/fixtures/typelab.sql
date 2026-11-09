-- Type lab: every Postgres type a Supabase project realistically uses, with edge-case rows
drop table if exists public.trips, public.cities, public.slugs, public.countries, public.type_lab, public.names_lab cascade;
drop type if exists public.mood, public.address cascade;
drop domain if exists public.positive_int, public.email_addr cascade;
create extension if not exists citext with schema extensions;
create extension if not exists hstore with schema extensions;
create extension if not exists ltree with schema extensions;
create extension if not exists postgis with schema extensions;
create extension if not exists vector with schema extensions;
set search_path = public, extensions;

create type public.mood as enum ('happy', 'sad', 'very, very angry', 'Đang vui');
create domain public.positive_int as integer check (value > 0);
create domain public.email_addr as extensions.citext check (value ~ '@');
create type public.address as (street text, city text, zip text);

create table public.type_lab (
  id            uuid primary key default gen_random_uuid(),
  label         text not null,
  -- integers
  c_smallint    smallint,
  c_integer     integer,
  c_bigint      bigint,
  c_bigint_huge bigint,
  c_identity    bigint generated always as identity,
  -- exact / float numbers
  c_numeric     numeric(10,2),
  c_numeric_big numeric,
  c_real        real,
  c_double      double precision,
  c_double_nan  double precision,
  c_money       money,
  -- booleans
  c_bool        boolean,
  -- strings
  c_text        text,
  c_varchar     varchar(20),
  c_char        char(6),
  c_citext      extensions.citext,
  c_long_text   text,
  -- uuid
  c_uuid        uuid,
  -- time
  c_date        date,
  c_time        time,
  c_timetz      time with time zone,
  c_timestamp   timestamp,
  c_timestamptz timestamptz,
  c_ts_inf      timestamptz,
  c_interval    interval,
  -- json
  c_json        json,
  c_jsonb       jsonb,
  c_jsonb_big   jsonb,
  -- arrays
  c_int_arr     integer[],
  c_text_arr    text[],
  c_2d_arr      integer[][],
  c_enum        public.mood,
  c_enum_arr    public.mood[],
  -- binary / network / misc
  c_bytea       bytea,
  c_inet        inet,
  c_cidr        cidr,
  c_macaddr     macaddr,
  c_bit         bit(8),
  c_xml         xml,
  c_tsvector    tsvector,
  c_int4range   int4range,
  c_tstzrange   tstzrange,
  -- geometric (built-in) and extensions
  c_point       point,
  c_polygon     polygon,
  c_geo_point   extensions.geometry(Point, 4326),
  c_geo_poly    extensions.geometry(Polygon, 4326),
  c_vector      extensions.vector(3),
  c_hstore      extensions.hstore,
  c_ltree       extensions.ltree,
  -- domains / composite / generated
  c_domain_int  public.positive_int,
  c_domain_mail public.email_addr unique,
  c_composite   public.address,
  c_generated   text generated always as (upper(label) || '#' || coalesce(c_integer, 0)) stored
);

-- tables for primary key types / foreign keys to non-"id" columns
create table public.countries (code text primary key, name text not null);           -- text PK
create table public.slugs (slug text primary key, title text);                        -- text PK with chars PocketBase forbids
create table public.cities (
  city_id integer generated always as identity primary key,                          -- identity PK not named id
  name text not null,
  country_code text not null references public.countries(code) on delete cascade,
  slug text references public.slugs(slug)
);
create table public.trips (id uuid primary key default gen_random_uuid(), city_id integer references public.cities(city_id), note text);

insert into public.countries values ('VN', 'Việt Nam'), ('US', 'United States'), ('jp', 'Japan');
insert into public.slugs values ('hello-world', 'ok slug'), ('Hà Nội/2026', 'slash + unicode'), ('a.b c', 'dot and space'), ('CON', 'windows reserved');
insert into public.cities (name, country_code, slug) values ('Hà Nội', 'VN', 'Hà Nội/2026'), ('Sài Gòn', 'VN', 'hello-world'), ('NYC', 'US', 'a.b c'), ('Tokyo', 'jp', 'CON');
insert into public.trips (city_id, note) select city_id, 'trip to ' || name from public.cities;

insert into public.type_lab (id, label, c_smallint, c_integer, c_bigint, c_bigint_huge, c_numeric, c_numeric_big, c_real, c_double, c_double_nan, c_money,
  c_bool, c_text, c_varchar, c_char, c_citext, c_long_text, c_uuid, c_date, c_time, c_timetz, c_timestamp, c_timestamptz, c_ts_inf, c_interval,
  c_json, c_jsonb, c_jsonb_big, c_int_arr, c_text_arr, c_2d_arr, c_enum, c_enum_arr, c_bytea, c_inet, c_cidr, c_macaddr, c_bit, c_xml, c_tsvector,
  c_int4range, c_tstzrange, c_point, c_polygon, c_geo_point, c_geo_poly, c_vector, c_hstore, c_ltree, c_domain_int, c_domain_mail, c_composite) values
('00000000-0000-4000-8000-000000000001', 'typical', 12, 123456, 9876543210, 42, 1234.56, 3.14159265358979323846264338327950288, 0.1, 0.1, 2.5, 19.99,
  true, 'Xin chào thế giới', 'varchar ok', 'ab', 'Hello@Example.com', repeat('Lorem ipsum ', 600), 'A0EEBC99-9C0B-4EF8-BB6D-6BB9BD380A11',
  '2024-02-29', '13:45:30.123456', '13:45:30+07', '2024-02-29 23:59:59.999999', '2024-02-29 23:59:59.999999+07', '2030-01-01 00:00:00+00', '1 year 2 mons 3 days 04:05:06.789',
  '{"b": 1, "a": 2, "a": 3}', '{"z": 1, "nested": {"arr": [1, 2.5, "x", null, true]}, "unicode": "Đặng 🎉"}', null,
  '{1,2,3}', '{"a","b,c","d\"e"}', '{{1,2},{3,4}}', 'happy', '{happy,sad}', '\xdeadbeef00ff'::bytea, '192.168.1.10/24', '10.0.0.0/8', '08:00:2b:01:02:03', B'10101010',
  '<note lang="vi">Chào</note>', to_tsvector('english', 'The quick brown foxes'), '[1,10)', '[2024-01-01 00:00+00,2024-12-31 00:00+00)', '(1.5,2.5)', '((0,0),(1,0),(1,1))',
  ST_SetSRID(ST_MakePoint(105.8342, 21.0278), 4326), ST_GeomFromText('POLYGON((105 21, 106 21, 106 22, 105 21))', 4326), '[0.1,0.2,0.3]', 'a=>1, b=>"x y"', 'Top.Science.Astronomy',
  5, 'Typical@Mail.com', row('1 Tràng Tiền', 'Hà Nội', '100000')),
('00000000-0000-4000-8000-000000000002', 'min / negative', -32768, -2147483648, -9223372036854775808, -9007199254740993, -99999999.99, -0.000000000000000000000000001, -3.4e38, -1.7976931348623157e308, -0.0, -92233720368547758.08,
  false, '', '', '', '', '', '00000000-0000-0000-0000-000000000000',
  '0001-01-01', '00:00:00', '00:00:00-12', '0001-01-01 00:00:00', '0001-01-01 00:00:00+00', '-infinity', '-178000000 years',
  '[]', '[]', null, '{}', '{}', '{}', 'sad', '{}', ''::bytea, '::1', '::/0', '00:00:00:00:00:00', B'00000000',
  '<a/>', to_tsvector(''), 'empty', 'empty', '(-1e10,-1e10)', '((0,0))',
  ST_SetSRID(ST_MakePoint(-180, -90), 4326), null, '[-1,-1,-1]', '', 'a',
  1, 'min@x.y', row(null, null, null)),
('00000000-0000-4000-8000-000000000003', 'max / extreme', 32767, 2147483647, 9223372036854775807, 9223372036854775807, 99999999.99, 123456789012345678901234567890.123456789, 3.4e38, 5e-324, 'NaN', 92233720368547758.07,
  true, repeat('x', 4999), repeat('y', 20), 'abcdef', 'MAX@EXAMPLE.COM', repeat('Đ', 120000), 'ffffffff-ffff-ffff-ffff-ffffffffffff',
  '9999-12-31', '24:00:00', '23:59:59.999999+14', '9999-12-31 23:59:59.999999', '9999-12-31 23:59:59.999999+00', 'infinity', '178000000 years',
  '{"big": 9007199254740993, "float": 0.1, "exp": 1e400}', '{"big": 9007199254740993, "float": 0.30000000000000004, "deep": [[[[[[[[[[1]]]]]]]]]]}',
  (select jsonb_agg(jsonb_build_object('i', g, 'pad', repeat('z', 100))) from generate_series(1, 12000) g),
  '{2147483647,-2147483648}', ('{"very long element ' || repeat('w', 300) || '"}')::text[], '{{1,2,3},{4,5,6},{7,8,9}}', 'very, very angry', '{"very, very angry",Đang vui,happy}',
  decode(repeat('ff', 70000), 'hex'), '2001:db8::ff00:42:8329', '2001:db8::/32', 'ff:ff:ff:ff:ff:ff', B'11111111',
  ('<r>' || repeat('<i>1</i>', 200) || '</r>')::xml, to_tsvector('simple', repeat('word ', 50)), '(,)', '(,)', '(1e300,1e300)', '((0,0),(10,0),(10,10),(0,10))',
  ST_SetSRID(ST_MakePoint(180, 90), 4326), null, '[1e30,-1e30,0]', 'k=>NULL', 'a.b.c.d.e.f.g.h',
  2147483647, 'Max@Mail.Com', row('Đường dài ' || repeat('x', 50), '東京', '100-0001')),
('00000000-0000-4000-8000-000000000004', 'all NULL', null, null, null, null, null, null, null, null, null, null,
  null, null, null, null, null, null, null,
  null, null, null, null, null, null, null,
  null, null, null, null, null, null, null, null, null, null, null, null, null, null, null,
  null, null, null, null, null, null, null, null, null,
  null, null, null),
('00000000-0000-4000-8000-000000000005', 'special values', 0, 0, 0, 0, 0, 'NaN', 'Infinity', '-Infinity', 'Infinity', 0,
  false, E'line1\nline2\ttab "quote" \\ backslash ''single''', ' leading/trailing ', 'a  ', 'ĐỨC@ví.dụ', E'Tiếng Việt NFD: Tiếng', '6ba7b810-9dad-11d1-80b4-00c04fd430c8',
  '1970-01-01', '12:00:00', '12:00:00+00', '1970-01-01 00:00:00', '1969-12-31 17:00:00-07', '2038-01-19 03:14:08+00', '-1 days +02:00:00',
  'null', 'null', '"just a string"', '{NULL,1,NULL}', '{NULL,"",NULL}', '{{NULL,1},{2,NULL}}', 'Đang vui', '{NULL,happy}', '\x00'::bytea, '0.0.0.0', '0.0.0.0/0', '01:23:45:67:89:ab', B'00001111',
  '<?xml version="1.0"?><x>&amp;&lt;</x>', to_tsvector('simple', 'Đặng Thị Ánh'), '[5,5]', '[2024-01-01 00:00+00,2024-01-01 00:00+00]', '(NaN,Infinity)', '((0,0),(0,0),(0,0))',
  ST_SetSRID(ST_MakePoint(0, 0), 4326), ST_GeomFromText('POLYGON EMPTY', 4326), '[0,0,0]', '"a b"=>"c d", "x"=>""', 'Top',
  42, 'ĐỨC@VÍ.DỤ.vn', row('', '', ''));

create table public.names_lab (id serial primary key, name text not null);
insert into public.names_lab (name) values ('banana'),('Apple'),('ápple'),('Zebra'),('zebra'),('Đà Lạt'),('Da Nang'),('đà nẵng');
