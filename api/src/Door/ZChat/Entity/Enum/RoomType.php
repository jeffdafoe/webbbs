<?php

namespace App\Door\ZChat\Entity\Enum;

enum RoomType: string
{
    case PUBLIC = 'public';
    case PRIVATE = 'private';
    case LOCKED = 'locked';
    case PERMANENT = 'permanent';
    case TEMPORARY = 'temporary';
}
